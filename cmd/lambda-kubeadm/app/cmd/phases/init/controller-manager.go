package phases

import (
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/phases/workflow"
)

func NewControllerManagerPhase() workflow.Phase {
	return workflow.Phase{
		Name:  "controller-manager",
		Short: "Controller Manager generation",
		Run:   runControllerManager,
	}
}

func runControllerManager(c workflow.RunData) error {
	klog.V(0).Infoln("[controller-manager] Starting phase")
	// For now, do nothing and return nil
	return nil
}