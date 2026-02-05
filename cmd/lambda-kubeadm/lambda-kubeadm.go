package main

import (
	"fmt"
	"os"

	"k8s.io/kubernetes/cmd/lambda-kubeadm/app"
)

func main() {
	if err := app.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
