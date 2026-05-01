/*
Copyright 2015 The Kubernetes Authors.

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

package config

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
)

// WaitForAPIServerSyncPeriod is the period between checks for the node API sync.
const WaitForAPIServerSyncPeriod = 1 * time.Second

// APIServerPollPeriod is how often the serverless kubelet refreshes pods assigned
// to this node. The Lambda apiserver intentionally does not support watch, so the
// kubelet uses whole-list snapshots instead.
const APIServerPollPeriod = 1 * time.Second

// NewSourceApiserver creates a config source that polls pods assigned to this
// node from the apiserver. This custom kubelet path is intentionally watch-free
// for serverless apiserver compatibility.
func NewSourceApiserver(c clientset.Interface, nodeName types.NodeName, nodeHasSynced func() bool, updates chan<- interface{}) {
	klog.InfoS("Waiting for node sync before polling apiserver pods")
	go func() {
		for {
			if nodeHasSynced() {
				klog.V(4).InfoS("node sync completed")
				break
			}
			time.Sleep(WaitForAPIServerSyncPeriod)
			klog.V(4).InfoS("node sync has not completed yet")
		}
		klog.InfoS("Polling apiserver for assigned pods", "period", APIServerPollPeriod)
		pollApiserverPods(context.Background(), c, nodeName, updates, APIServerPollPeriod)
	}()
}

func pollApiserverPods(ctx context.Context, c clientset.Interface, nodeName types.NodeName, updates chan<- interface{}, period time.Duration) {
	wait.UntilWithContext(ctx, func(ctx context.Context) {
		if err := pollApiserverPodsOnce(ctx, c, nodeName, updates); err != nil {
			klog.ErrorS(err, "Failed polling apiserver pods", "node", nodeName)
		}
	}, period)
}

func pollApiserverPodsOnce(ctx context.Context, c clientset.Interface, nodeName types.NodeName, updates chan<- interface{}) error {
	pods, err := listAssignedPods(ctx, c, nodeName)
	if err != nil {
		return err
	}
	updates <- kubetypes.PodUpdate{Pods: pods, Op: kubetypes.SET, Source: kubetypes.ApiserverSource}
	return nil
}

func listAssignedPods(ctx context.Context, c clientset.Interface, nodeName types.NodeName) ([]*v1.Pod, error) {
	podList, err := c.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector:   fields.OneTermEqualSelector("spec.nodeName", string(nodeName)).String(),
		ResourceVersion: "0",
	})
	if err != nil {
		return nil, err
	}

	pods := make([]*v1.Pod, 0, len(podList.Items))
	for i := range podList.Items {
		pods = append(pods, podList.Items[i].DeepCopy())
	}
	return pods, nil
}
