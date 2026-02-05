/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmd

import (
	"io"

	"github.com/spf13/cobra"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/phases/workflow"
	phases "k8s.io/kubernetes/cmd/lambda-kubeadm/app/cmd/phases/init"
)

// compile-time assert that the local data object satisfies the phases data interface.
var _ phases.InitData = &initData{}

type initData struct {
	cfgYaml string
}


type initOptions struct{}

// newCmdInit returns a stub init command for lambda-kubeadm.
func newCmdInit(out io.Writer, initOptions *initOptions) *cobra.Command {
	if initOptions == nil {
		initOptions = newInitOptions()
	}
	initRunner := workflow.NewRunner()
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a serverless Kubernetes control plane",
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
	}
	cmd.SetOut(out)
	initRunner.AppendPhase(phases.NewPreflightPhase())
	initRunner.AppendPhase(phases.NewCertsPhase())


	// set the data in the runner
	initRunner.SetDataInitializer(func(cmd *cobra.Command, args []string) (workflow.RunData, error) {
		return newInitData(), nil
	})

	initRunner.BindToCommand(cmd)
	return cmd
}

func newInitOptions() *initOptions {
	return &initOptions{}
}

func newInitData() phases.InitData {
	return &initData{
		cfgYaml: "",
	}
}

func (d *initData) CfgYaml() string {
	return d.cfgYaml
}
