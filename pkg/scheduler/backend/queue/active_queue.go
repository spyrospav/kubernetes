/*
Copyright 2024 The Kubernetes Authors.

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

package queue

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"k8s.io/kubernetes/pkg/scheduler/backend/heap"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/awsstore"
	_ "k8s.io/kubernetes/pkg/scheduler/backend/heap"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
)

// activeQueuer is a wrapper for activeQ related operations.
// Its methods, except "unlocked" ones, take the lock inside.
// Note: be careful when using unlocked() methods.
// getLock() methods should be used only for unlocked() methods
// and it is forbidden to call any other activeQueuer's method under this lock.
type activeQueuer interface {
	underLock(func(unlockedActiveQ unlockedActiveQueuer))
	underRLock(func(unlockedActiveQ unlockedActiveQueueReader))

	update(newPod *v1.Pod, oldPodInfo *framework.QueuedPodInfo) *framework.QueuedPodInfo
	delete(pInfo *framework.QueuedPodInfo) error
	pop(logger klog.Logger) (*framework.QueuedPodInfo, error)
	list() []*v1.Pod
	len() int
	has(pInfo *framework.QueuedPodInfo) bool

	schedulingCycle() int64
	close()
	broadcast()
}

// unlockedActiveQueuer defines activeQ methods that are not protected by the lock itself.
// underLock() method should be used to protect these methods.
type unlockedActiveQueuer interface {
	unlockedActiveQueueReader
	// add adds a new pod to the activeQ.
	// The event should show which event triggered this addition and is used for the metric recording.
	// This method should be called in activeQueue.underLock().
	add(pInfo *framework.QueuedPodInfo, event string)
}

// unlockedActiveQueueReader defines activeQ read-only methods that are not protected by the lock itself.
// underLock() or underRLock() method should be used to protect these methods.
type unlockedActiveQueueReader interface {
	// get returns the pod matching pInfo inside the activeQ.
	// Returns false if the pInfo doesn't exist in the queue.
	// This method should be called in activeQueue.underLock() or activeQueue.underRLock().
	get(pInfo *framework.QueuedPodInfo) (*framework.QueuedPodInfo, bool)
	// has returns if pInfo exists in the queue.
	// This method should be called in activeQueue.underLock() or activeQueue.underRLock().
	has(pInfo *framework.QueuedPodInfo) bool
}

// unlockedActiveQueue defines activeQ methods that are not protected by the lock itself.
// activeQueue.underLock() or activeQueue.underRLock() method should be used to protect these methods.
type unlockedActiveQueue struct {
	queue awsstore.PodQueue
}

func newUnlockedActiveQueue(queue awsstore.PodQueue) *unlockedActiveQueue {
	return &unlockedActiveQueue{
		queue: queue,
	}
}

// add adds a new pod to the activeQ.
// The event should show which event triggered this addition and is used for the metric recording.
// This method should be called in activeQueue.underLock().
func (uaq *unlockedActiveQueue) add(pInfo *framework.QueuedPodInfo, event string) {
	uaq.queue.AddOrUpdate(pInfo)
	metrics.SchedulerQueueIncomingPods.WithLabelValues("active", event).Inc()
}

// get returns the pod matching pInfo inside the activeQ.
// Returns false if the pInfo doesn't exist in the queue.
// This method should be called in activeQueue.underLock() or activeQueue.underRLock().
func (uaq *unlockedActiveQueue) get(pInfo *framework.QueuedPodInfo) (*framework.QueuedPodInfo, bool) {
	return uaq.queue.Get(pInfo)
}

// has returns if pInfo exists in the queue.
// This method should be called in activeQueue.underLock() or activeQueue.underRLock().
func (uaq *unlockedActiveQueue) has(pInfo *framework.QueuedPodInfo) bool {
	return uaq.queue.Has(pInfo)
}

// backoffQPopper defines method that is used to pop from the backoffQ when the activeQ is empty.
type backoffQPopper interface {
	// popBackoff pops the pInfo from the podBackoffQ.
	popBackoff() (*framework.QueuedPodInfo, error)
	// len returns length of the podBackoffQ queue.
	lenBackoff() int
}

// activeQueue implements activeQueuer. All of the fields have to be protected using the lock.
type activeQueue struct {
	// lock synchronizes all operations related to activeQ.
	// It protects activeQ, inFlightPods, inFlightEvents, schedulingCycle and closed fields.
	// Caution: DO NOT take "SchedulingQueue.lock" after taking "lock".
	// You should always take "SchedulingQueue.lock" first, otherwise the queue could end up in deadlock.
	// "lock" should not be taken after taking "backoffQueue.lock" or "nominator.nLock".
	// Correct locking order is: SchedulingQueue.lock > lock > backoffQueue.lock > nominator.nLock.
	lock sync.RWMutex

	// activeQ is heap structure that scheduler actively looks at to find pods to
	// schedule. Head of heap is the highest priority pod.
	queue awsstore.PodQueue

	// unlockedQueue is a wrapper of queue providing methods that are not locked themselves
	// and can be used in the underLock() or underRLock().
	unlockedQueue *unlockedActiveQueue

	// cond is a condition that is notified when the pod is added to activeQ.
	// When SchedulerPopFromBackoffQ feature is enabled,
	// condition is also notified when the pod is added to backoffQ.
	// It is used with lock.
	cond sync.Cond

	// schedCycle represents sequence number of scheduling cycle and is incremented
	// when a pod is popped.
	schedCycle int64

	// closed indicates that the queue is closed.
	// It is mainly used to let Pop() exit its control loop while waiting for an item.
	closed bool

	// isSchedulingQueueHintEnabled indicates whether the feature gate for the scheduling queue is enabled.
	//isSchedulingQueueHintEnabled bool

	metricsRecorder metrics.MetricAsyncRecorder
}

func newActiveQueueWithBackend(
	metricRecorder metrics.MetricAsyncRecorder,
	lessFn framework.LessFunc,
	useDynamo bool,
	awsCfg aws.Config,
) *activeQueue {
	var queue awsstore.PodQueue
	if useDynamo {
		ctx := context.Background()

		// FIXME: Use the local DynamoDB endpoint for testing.
		//dynaLocalURL := "http://host.docker.internal:8000"
		localOpt := func(o *dynamodb.Options) {
			//o.BaseEndpoint = aws.String(dynaLocalURL)
		}

		activeConfig := awsstore.Config{
			Backend:   awsstore.BackendDynamoDB,
			TableName: "scheduler-activeq",
			QueueID:   "activeQ",
		}

		remotePQ, err := awsstore.NewPriorityQueueAWS(
			ctx,
			awsCfg,
			activeConfig,
			[]func(*dynamodb.Options){localOpt},
			nil)
		if err != nil {
			panic(fmt.Sprintf("activeQ AWS backend: %v", err))
		}

		queue = awsstore.DDBPQ{Ctx: context.Background(), Aws: remotePQ}
	} else {
		h := heap.NewWithRecorder(podInfoKeyFunc,
			heap.LessFunc[*framework.QueuedPodInfo](lessFn),
			metrics.NewActivePodsRecorder())
		queue = awsstore.HeapPQ{H: h}
	}

	if queue == nil {
		panic("activeQ queue is nil")
	}

	aq := &activeQueue{
		queue:           queue,
		metricsRecorder: metricRecorder,
		unlockedQueue:   newUnlockedActiveQueue(queue),
	}
	aq.cond.L = &aq.lock

	return aq
}

// underLock runs the fn function under the lock.Lock.
// fn can run unlockedActiveQueuer methods but should NOT run any other activeQueue method,
// as it would end up in deadlock.
func (aq *activeQueue) underLock(fn func(unlockedActiveQ unlockedActiveQueuer)) {
	aq.lock.Lock()
	defer aq.lock.Unlock()
	fn(aq.unlockedQueue)
}

// underLock runs the fn function under the lock.RLock.
// fn can run unlockedActiveQueueReader methods but should NOT run any other activeQueue method,
// as it would end up in deadlock.
func (aq *activeQueue) underRLock(fn func(unlockedActiveQ unlockedActiveQueueReader)) {
	aq.lock.RLock()
	defer aq.lock.RUnlock()
	fn(aq.unlockedQueue)
}

// update updates the pod in activeQ if oldPodInfo is already in the queue.
// It returns new pod info if updated, nil otherwise.
func (aq *activeQueue) update(newPod *v1.Pod, oldPodInfo *framework.QueuedPodInfo) *framework.QueuedPodInfo {
	aq.lock.Lock()
	defer aq.lock.Unlock()

	if pInfo, exists := aq.queue.Get(oldPodInfo); exists {
		_ = pInfo.Update(newPod)
		aq.queue.AddOrUpdate(pInfo)
		return pInfo
	}
	return nil
}

// delete deletes the pod info from activeQ.
func (aq *activeQueue) delete(pInfo *framework.QueuedPodInfo) error {
	aq.lock.Lock()
	defer aq.lock.Unlock()

	return aq.queue.Delete(pInfo)
}

// pop removes the head of the queue and returns it.
// It blocks if the queue is empty and waits until a new item is added to the queue.
// It increments scheduling cycle when a pod is popped.
func (aq *activeQueue) pop(logger klog.Logger) (*framework.QueuedPodInfo, error) {
	aq.lock.Lock()
	defer aq.lock.Unlock()

	return aq.unlockedPop(logger)
}

func (aq *activeQueue) unlockedPop(logger klog.Logger) (*framework.QueuedPodInfo, error) {
	var pInfo *framework.QueuedPodInfo
	for aq.queue.Len() == 0 {
		// When the queue is empty, invocation of Pop() is blocked until new item is enqueued.
		// When Close() is called, the p.closed is set and the condition is broadcast,
		// which causes this loop to continue and return from the Pop().
		if aq.closed {
			logger.V(2).Info("Scheduling queue is closed")
			return nil, nil
		}
		aq.cond.Wait()
	}

	logger.V(4).Info("Scheduling queue is ready to pop", "activeQLen", aq.queue.Len(), "backoffQLen", 0)

	pInfo, err := aq.queue.Pop()
	if err != nil {
		return nil, err
	}
	pInfo.Attempts++
	pInfo.BackoffExpiration = time.Time{}
	aq.schedCycle++

	// Update metrics and reset the set of unschedulable plugins for the next attempt.
	for plugin := range pInfo.UnschedulablePlugins.Union(pInfo.PendingPlugins) {
		metrics.UnschedulableReason(plugin, pInfo.Pod.Spec.SchedulerName).Dec()
	}
	pInfo.UnschedulablePlugins.Clear()
	pInfo.PendingPlugins.Clear()

	return pInfo, nil
}

// list returns all pods that are in the queue.
func (aq *activeQueue) list() []*v1.Pod {
	aq.lock.RLock()
	defer aq.lock.RUnlock()
	var result []*v1.Pod
	for _, pInfo := range aq.queue.List() {
		result = append(result, pInfo.Pod)
	}
	return result
}

// len returns length of the queue.
func (aq *activeQueue) len() int {
	return aq.queue.Len()
}

// has inform if pInfo exists in the queue.
func (aq *activeQueue) has(pInfo *framework.QueuedPodInfo) bool {
	aq.lock.RLock()
	defer aq.lock.RUnlock()
	return aq.queue.Has(pInfo)
}

func (aq *activeQueue) schedulingCycle() int64 {
	aq.lock.RLock()
	defer aq.lock.RUnlock()
	return aq.schedCycle
}

// close closes the activeQueue.
func (aq *activeQueue) close() {
	aq.lock.Lock()
	defer aq.lock.Unlock()
	aq.closed = true
}

// broadcast notifies the pop() operation that new pod(s) was added to the activeQueue.
func (aq *activeQueue) broadcast() {
	aq.cond.Broadcast()
}
