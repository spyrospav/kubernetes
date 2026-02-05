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
)

type resetOptions struct{}

// newCmdReset returns a stub reset command for lambda-kubeadm.
func newCmdReset(in io.Reader, out io.Writer, _ *resetOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Revert changes made to a node by lambda-kubeadm",
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
	}
	cmd.SetIn(in)
	cmd.SetOut(out)
	return cmd
}
