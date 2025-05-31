package main

import (
	"context"
	"flag"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/kubernetes/pkg/controller/podgc"
	"path/filepath"
)

func main() {
	// ------- flags ----------
	threshold := flag.Int("terminated-pod-gc-threshold", 2,
		"Number of terminated pods that can exist before they are garbage-collected.")
	flag.Parse()
	// ------------------------

	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err.Error())
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	factory := informers.NewSharedInformerFactory(clientset, 0)

	// pods, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
	ctx := context.TODO()

	gc := podgc.NewPodGC(
		ctx, clientset,
		factory.Core().V1().Pods(),
		factory.Core().V1().Nodes(),
		*threshold,
	)

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	go gc.Run(ctx)
	<-ctx.Done()
}
