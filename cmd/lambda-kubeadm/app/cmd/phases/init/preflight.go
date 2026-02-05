package phases

import (
	"errors"
	"fmt"

	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/options"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/phases/workflow"
	cmdutil "k8s.io/kubernetes/cmd/kubeadm/app/cmd/util"
)

var (
	preflightExample = cmdutil.Examples(`
		# Run pre-flight checks for kubeadm init using a config file.
		lambda-kubeadm init phase preflight --config kubeadm-config.yaml
		`)
)

// NewPreflightPhase creates a kubeadm workflow phase that implements preflight checks for a new control-plane node.
func NewPreflightPhase() workflow.Phase {
	return workflow.Phase{
		Name:    "preflight",
		Short:   "Run pre-flight checks",
		Long:    "Run pre-flight checks for kubeadm init.",
		Example: preflightExample,
		Run:     runPreflight,
		InheritFlags: []string{
			options.CfgPath,
			options.ImageRepository,
			options.NodeCRISocket,
			options.IgnorePreflightErrors,
			options.DryRun,
		},
	}
}

func runPreflight(c workflow.RunData) error {
	data, ok := c.(InitData)
	// PRINT data
	fmt.Println(data)
	if !ok {
		return errors.New("preflight phase invoked with an invalid data struct")
	}
	fmt.Println("[preflight] Running pre-flight checks")

	//TODO: implement preflight checks for lambda-kubeadm

	
	return nil
}