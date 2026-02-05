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
	"os/exec"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

// newCmdConfigPrint returns the "lambda-kubeadm config print" command.
func newCmdConfigPrint(out io.Writer) *cobra.Command {
	var s3Path string

	cmd := &cobra.Command{
		Use:   "print",
		Short: "Print cluster config stored in S3",
		RunE: func(cmd *cobra.Command, args []string) error {
			if s3Path == "" {
				return errors.New("--s3-path is required")
			}
			if !strings.HasPrefix(s3Path, "s3://") {
				return errors.New("--s3-path must be an s3 URI, e.g. s3://bucket/path")
			}

			execCmd := exec.Command("aws", "s3", "cp", s3Path, "-")
			execCmd.Stdout = out
			execCmd.Stderr = cmd.ErrOrStderr()
			return execCmd.Run()
		},
		Args: cobra.NoArgs,
	}

	cmd.Flags().StringVar(&s3Path, "s3-path", "", "S3 URI to the config, e.g. s3://bucket/path/config.yaml")
	return cmd
}
