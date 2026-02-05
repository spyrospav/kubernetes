package cmd

import (
	"io"

	"github.com/lithammer/dedent"
	"github.com/spf13/cobra"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/options"
)

// NewLambdaKubeadmCommand returns cobra.Command to run lambda-kubeadm command
func NewLambdaKubeadmCommand(in io.Reader, out, err io.Writer) *cobra.Command {
	var rootfsPath string

	cmds := &cobra.Command{
		Use:   "lambda-kubeadm",
		Short: "lambda-kubeadm: easily bootstrap a serverless Kubernetes cluster",
		Long: dedent.Dedent(`

			    ┌───────────────────────────────────────────────────────────┐
			    │ LAMBDA-KUBEADM                                            │
			    │ Easily bootstrap a serverless Kubernetes cluster          │
			    └───────────────────────────────────────────────────────────┘

			Example usage:

			    Create a cluster with a serverless control plane
			    (which controls the cluster), and one worker node
			    (where your workloads, like Pods and Deployments run).

			    ┌───────────────────────────────────────────────────────────┐
			    │ On your local machine:                                    │
			    ├───────────────────────────────────────────────────────────┤
			    │ control-plane# lambda-kubeadm init                        │
			    └───────────────────────────────────────────────────────────┘

			    ┌───────────────────────────────────────────────────────────┐
			    │ On your worker machine:                                   │
			    ├───────────────────────────────────────────────────────────┤
			    │ worker# lambda-kubeadm join <arguments-returned-from-init>|
			    └───────────────────────────────────────────────────────────┘

			    You can then repeat the second step on as many other machines as you like.

		`),
		SilenceErrors: true,
		SilenceUsage:  true,
		// PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// 	if rootfsPath != "" {
		// 		if err := kubeadmutil.Chroot(rootfsPath); err != nil {
		// 			return err
		// 		}
		// 	}
		// 	return nil
		// },
	}

	cmds.ResetFlags()

	// cmds.AddCommand(newCmdCertsUtility(out))
	cmds.AddCommand(newCmdConfig(out)) // print ✔, TODO: add other config subcommands
	cmds.AddCommand(newCmdInit(out, nil))
	cmds.AddCommand(newCmdJoin(out, nil))
	cmds.AddCommand(newCmdReset(in, out, nil))
	cmds.AddCommand(newCmdVersion(out)) // ✔
	// cmds.AddCommand(newCmdToken(out, err))
	options.AddKubeadmOtherFlags(cmds.PersistentFlags(), &rootfsPath)

	return cmds
}
