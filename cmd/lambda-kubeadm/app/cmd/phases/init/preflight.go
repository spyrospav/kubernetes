package phases

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"

	"k8s.io/klog/v2"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/options"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/phases/workflow"
	cmdutil "k8s.io/kubernetes/cmd/kubeadm/app/cmd/util"
	"sigs.k8s.io/yaml"
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
			"template-path",
		},
	}
}

func runPreflight(c workflow.RunData) error {
	data, ok := c.(InitData)
	// PRINT data
	klog.V(1).Infoln("[preflight] Data: ", data)
	if !ok {
		return errors.New("preflight phase invoked with an invalid data struct")
	}
	klog.V(0).Infoln("[preflight] Running pre-flight checks")

	if _, err := exec.LookPath("sam"); err != nil {
		return errors.New("sam is not installed or not in PATH")
	}

	// Create a template.yaml file that will be used by sam to deploy the serverless control plane
	if err := writeSamTemplate(data.TemplatePath()); err != nil {
		return err
	}

	//TODO: implement preflight checks for lambda-kubeadm

	
	return nil
}

func writeSamTemplate(path string) error {
	if path == "" {
		path = "template.yaml"
	}
	klog.V(0).Infoln("[preflight] Writing SAM template")
	path = filepath.Clean(path)
	template := map[string]any{
		"AWSTemplateFormatVersion": "2010-09-09",
		"Transform":                "AWS::Serverless-2016-10-31",
		"Description":              "Serverless control plane template",
		"Resources":                map[string]any{},
	}
	content, err := yaml.Marshal(template)
	if err != nil {
		return err
	}

	return os.WriteFile(path, content, 0o644)
}