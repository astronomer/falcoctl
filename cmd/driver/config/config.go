// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2023 The Falco Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package driverconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/net/context"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/falcosecurity/falcoctl/internal/config"
	"github.com/falcosecurity/falcoctl/internal/utils"
	drivertype "github.com/falcosecurity/falcoctl/pkg/driver/type"
	"github.com/falcosecurity/falcoctl/pkg/options"
)

const (
	configMapEngineKindKey = "engine.kind"
	longConfig             = `Configure a driver for future usages with other driver subcommands.
It will also update local Falco configuration or k8s configmap depending on the environment where it is running, to let Falco use chosen driver.
Only supports deployments of Falco that use a driver engine, ie: one between kmod, ebpf and modern-ebpf.
If engine.kind key is set to a non-driver driven engine, Falco configuration won't be touched.
`
)

type driverConfigOptions struct {
	*options.Common
	*options.Driver
	Update     bool
	Namespace  string
	KubeConfig string
}

// NewDriverConfigCmd configures a driver and stores it in config.
func NewDriverConfigCmd(ctx context.Context, opt *options.Common, driver *options.Driver) *cobra.Command {
	o := driverConfigOptions{
		Common: opt,
		Driver: driver,
	}

	cmd := &cobra.Command{
		Use:                   "config [flags]",
		DisableFlagsInUseLine: true,
		Short:                 "Configure a driver",
		Long:                  longConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.RunDriverConfig(ctx)
		},
	}

	cmd.Flags().BoolVar(&o.Update, "update-falco", true, "Whether to update Falco config/configmap.")
	cmd.Flags().StringVar(&o.Namespace, "namespace", "", "Kubernetes namespace.")
	cmd.Flags().StringVar(&o.KubeConfig, "kubeconfig", "", "Kubernetes config.")
	return cmd
}

// RunDriverConfig implements the driver configuration command.
func (o *driverConfigOptions) RunDriverConfig(ctx context.Context) error {
	o.Printer.Logger.Info("Running falcoctl driver config", o.Printer.Logger.Args(
		"name", o.Driver.Name,
		"version", o.Driver.Version,
		"type", o.Driver.Type.String(),
		"host-root", o.Driver.HostRoot,
		"repos", strings.Join(o.Driver.Repos, ",")))

	if o.Update {
		if err := o.commit(ctx, o.Driver.Type); err != nil {
			return err
		}
	}
	return config.StoreDriver(o.Driver.ToDriverConfig(), o.ConfigFile)
}

func checkFalcoRunsWithDrivers(engineKind string) error {
	// Modify the data in the ConfigMap/Falco config file ONLY if engine.kind is set to a known driver type.
	// This ensures that we modify the config only for Falcos running with drivers, and not plugins/gvisor.
	// Scenario: user has multiple Falco pods deployed in its cluster, one running with driver,
	// other running with plugins. We must only touch the one running with driver.
	if _, err := drivertype.Parse(engineKind); err != nil {
		return fmt.Errorf("engine.kind is not driver driven: %s", engineKind)
	}
	return nil
}

func (o *driverConfigOptions) replaceDriverTypeInFalcoConfig(driverType drivertype.DriverType) error {
	falcoCfgFile := filepath.Clean(filepath.Join(string(os.PathSeparator), "etc", "falco", "falco.yaml"))
	type engineCfg struct {
		Kind string `yaml:"kind"`
	}
	type falcoCfg struct {
		Engine engineCfg `yaml:"engine"`
	}
	yamlFile, err := os.ReadFile(filepath.Clean(falcoCfgFile))
	if err != nil {
		return err
	}
	cfg := falcoCfg{}
	if err = yaml.Unmarshal(yamlFile, &cfg); err != nil {
		return err
	}
	if err = checkFalcoRunsWithDrivers(cfg.Engine.Kind); err != nil {
		o.Printer.Logger.Warn("Avoid updating Falco configuration",
			o.Printer.Logger.Args("config", falcoCfgFile, "reason", err))
		return nil
	}
	const configKindKey = "kind: "
	return utils.ReplaceTextInFile(falcoCfgFile, configKindKey+cfg.Engine.Kind, configKindKey+driverType.String(), 1)
}

func (o *driverConfigOptions) replaceDriverTypeInK8SConfigMap(ctx context.Context, driverType drivertype.DriverType) error {
	var (
		err error
		cfg *rest.Config
	)

	if o.KubeConfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", o.KubeConfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return err
	}

	cl, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}

	configMapList, err := cl.CoreV1().ConfigMaps(o.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/instance: falco",
	})
	if err != nil {
		return err
	}
	if configMapList.Size() == 0 {
		return errors.New(`no configmaps matching "app.kubernetes.io/instance: falco" label were found`)
	}

	type patchDriverTypeValue struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value string `json:"value"`
	}
	payload := []patchDriverTypeValue{{
		Op:    "replace",
		Path:  "/data/" + configMapEngineKindKey,
		Value: driverType.String(),
	}}
	plBytes, _ := json.Marshal(payload)

	for i := 0; i < configMapList.Size(); i++ {
		configMap := configMapList.Items[i]
		currEngineKind := configMap.Data[configMapEngineKindKey]
		if err = checkFalcoRunsWithDrivers(currEngineKind); err != nil {
			o.Printer.Logger.Warn("Avoid updating Falco configMap",
				o.Printer.Logger.Args("configMap", configMap.Name, "reason", err))
			continue
		}
		// Patch the configMap
		if _, err = cl.CoreV1().ConfigMaps(configMap.Namespace).Patch(
			ctx, configMap.Name, types.JSONPatchType, plBytes, metav1.PatchOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// commit saves the updated driver type to Falco config,
// either to the local falco.yaml or updating the deployment configmap.
func (o *driverConfigOptions) commit(ctx context.Context, driverType drivertype.DriverType) error {
	if o.Namespace != "" {
		// Ok we are on k8s
		return o.replaceDriverTypeInK8SConfigMap(ctx, driverType)
	}
	return o.replaceDriverTypeInFalcoConfig(driverType)
}
