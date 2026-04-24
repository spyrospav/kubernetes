/*
Copyright 2014 The Kubernetes Authors.

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
	"testing"

	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
)

func TestPollApiserverPodsOnceSendsAssignedPods(t *testing.T) {
	pod1 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "one"},
		Spec: v1.PodSpec{
			NodeName:   "node-a",
			Containers: []v1.Container{{Image: "image/one"}},
		},
	}
	pod2 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "q", Namespace: "two"},
		Spec: v1.PodSpec{
			NodeName:   "node-a",
			Containers: []v1.Container{{Image: "image/two"}},
		},
	}
	otherNodePod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "three"},
		Spec: v1.PodSpec{
			NodeName:   "node-b",
			Containers: []v1.Container{{Image: "image/three"}},
		},
	}

	client := fake.NewSimpleClientset(otherNodePod)
	client.PrependReactor("list", "pods", func(action core.Action) (bool, runtime.Object, error) {
		listAction := action.(core.ListAction)
		if got, want := listAction.GetListRestrictions().Fields.String(), "spec.nodeName=node-a"; got != want {
			t.Fatalf("expected field selector %q, got %q", want, got)
		}
		return true, &v1.PodList{Items: []v1.Pod{*pod1, *pod2}}, nil
	})
	ch := make(chan interface{}, 1)

	if err := pollApiserverPodsOnce(context.Background(), client, types.NodeName("node-a"), ch); err != nil {
		t.Fatalf("pollApiserverPodsOnce returned error: %v", err)
	}

	got := (<-ch).(kubetypes.PodUpdate)
	expected := kubetypes.PodUpdate{
		Pods:   []*v1.Pod{pod1, pod2},
		Op:     kubetypes.SET,
		Source: kubetypes.ApiserverSource,
	}
	if !apiequality.Semantic.DeepEqual(expected, got) {
		t.Fatalf("expected %#v, got %#v", expected, got)
	}
}

func TestPollApiserverPodsOnceInitialEmptySendsEmptyPodUpdate(t *testing.T) {
	client := fake.NewSimpleClientset()
	ch := make(chan interface{}, 1)

	if err := pollApiserverPodsOnce(context.Background(), client, types.NodeName("node-a"), ch); err != nil {
		t.Fatalf("pollApiserverPodsOnce returned error: %v", err)
	}

	got := (<-ch).(kubetypes.PodUpdate)
	expected := kubetypes.PodUpdate{
		Pods:   []*v1.Pod{},
		Op:     kubetypes.SET,
		Source: kubetypes.ApiserverSource,
	}
	if !apiequality.Semantic.DeepEqual(expected, got) {
		t.Fatalf("expected %#v, got %#v", expected, got)
	}
}

func TestPollApiserverPodsOnceReturnsListError(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "pods", func(action core.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver unavailable")
	})
	ch := make(chan interface{}, 1)

	if err := pollApiserverPodsOnce(context.Background(), client, types.NodeName("node-a"), ch); err == nil {
		t.Fatalf("expected list error, got nil")
	}
	if len(ch) != 0 {
		t.Fatalf("expected no update on list error")
	}
}
