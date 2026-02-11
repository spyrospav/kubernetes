package phases

import (
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/options"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/phases/workflow"
)

func NewAPIServerPhase() workflow.Phase {
	return workflow.Phase{
		Name:  "apiserver",
		Short: "API Server generation",
		Phases: []workflow.Phase{
			{
				Name:         "flags",
				Short:        "Inherit API server flags",
				InheritFlags: getAPIServerFlags(),
				Run:          runAPIServerFlags,
			},
			{
				Name:  "manifest",
				Short: "Build API server Lambda manifest",
				Run:   runAPIServerManifest,
			},
			{
				Name:  "deploy",
				Short: "Deploy API server manifest to Lambda",
				Run:   runAPIServerDeploy,
			},
		},
		Run: runAPIServer,
	}
}

func getAPIServerFlags() []string {
	return []string{
		options.CfgPath,
		options.CertificatesDir,
		options.KubernetesVersion,
		options.ImageRepository,
		options.Patches,
		options.DryRun,
		options.APIServerAdvertiseAddress,
		options.ControlPlaneEndpoint,
		options.APIServerBindPort,
		options.APIServerExtraArgs,
		options.FeatureGatesString,
		options.NetworkingServiceSubnet,
	}
}

func runAPIServer(c workflow.RunData) error {
	klog.V(0).Infoln("[apiserver] Starting phase")
	return nil
}

func runAPIServerFlags(c workflow.RunData) error {
	klog.V(0).Infoln("[apiserver] Inheriting API server flags")
	return nil
}

func runAPIServerManifest(c workflow.RunData) error {
	klog.V(0).Infoln("[apiserver] Building API server Lambda manifest")
	return nil
}

func runAPIServerDeploy(c workflow.RunData) error {
	klog.V(0).Infoln("[apiserver] Deploying API server manifest to Lambda")
	return nil
}