package phases

import "k8s.io/kubernetes/cmd/kubeadm/app/cmd/phases/workflow"


func NewCertsPhase() workflow.Phase {
	return workflow.Phase{
		Name:   "certs",
		Short:  "Certificate generation",
		Phases: newCertSubPhases(),
		Run:    runCerts,
	}
}

func newCertSubPhases() []workflow.Phase {
	// return an empty Phase for now
	return []workflow.Phase{}
}

func runCerts(c workflow.RunData) error {
	return nil
}