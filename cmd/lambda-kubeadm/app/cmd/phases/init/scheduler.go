package phases

import (
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/phases/workflow"
)

func NewSchedulerPhase() workflow.Phase {
	return workflow.Phase{
		Name:  "scheduler",
		Short: "Scheduler generation",
		Run:   runScheduler,
	}
}

func runScheduler(c workflow.RunData) error {
	klog.V(0).Infoln("[scheduler] Starting phase")
	// For now, do nothing and return nil
	return nil
}