/*
Copyright 2017 The Kubernetes Authors.

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

// This file contains structures that implement scheduling queue types.
// Scheduling queues hold pods waiting to be scheduled. This file implements a
// priority queue which has two sub queues and a additional data structure,
// namely: activeQ, backoffQ and unschedulablePods.
// - activeQ holds pods that are being considered for scheduling.
// - backoffQ holds pods that moved from unschedulablePods and will move to
//   activeQ when their backoff periods complete.
// - unschedulablePods holds pods that were already attempted for scheduling and
//   are currently determined to be unschedulable.

package queue

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubernetes/pkg/scheduler/awsstore"
	"reflect"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/names"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/podtopologyspread"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	"k8s.io/kubernetes/pkg/scheduler/util"
	"k8s.io/utils/clock"
)

const (
	// DefaultPodMaxInUnschedulablePodsDuration is the default value for the maximum
	// time a pod can stay in unschedulablePods. If a pod stays in unschedulablePods
	// for longer than this value, the pod will be moved from unschedulablePods to
	// backoffQ or activeQ. If this value is empty, the default value (5min)
	// will be used.
	DefaultPodMaxInUnschedulablePodsDuration time.Duration = 5 * time.Minute
	// Scheduling queue names
	activeQ           = "Active"
	backoffQ          = "Backoff"
	unschedulablePods = "Unschedulable"

	preEnqueue = "PreEnqueue"
)

const (
	// DefaultPodInitialBackoffDuration is the default value for the initial backoff duration
	// for unschedulable pods. To change the default podInitialBackoffDurationSeconds used by the
	// scheduler, update the ComponentConfig value in defaults.go
	DefaultPodInitialBackoffDuration time.Duration = 1 * time.Second
	// DefaultPodMaxBackoffDuration is the default value for the max backoff duration
	// for unschedulable pods. To change the default podMaxBackoffDurationSeconds used by the
	// scheduler, update the ComponentConfig value in defaults.go
	DefaultPodMaxBackoffDuration time.Duration = 10 * time.Second
)

// PreEnqueueCheck is a function type. It's used to build functions that
// run against a Pod and the caller can choose to enqueue or skip the Pod
// by the checking result.
type PreEnqueueCheck func(pod *v1.Pod) bool

// SchedulingQueue is an interface for a queue to store pods waiting to be scheduled.
// The interface follows a pattern similar to cache.FIFO and cache.Heap and
// makes it easy to use those data structures as a SchedulingQueue.
type SchedulingQueue interface {
	framework.PodNominator
	Add(logger klog.Logger, pod *v1.Pod)
	// Activate moves the given pods to activeQ.
	// If a pod isn't found in unschedulablePods or backoffQ and it's in-flight,
	// the wildcard event is registered so that the pod will be requeued when it comes back.
	// But, if a pod isn't found in unschedulablePods or backoffQ and it's not in-flight (i.e., completely unknown pod),
	// Activate would ignore the pod.
	Activate(logger klog.Logger, pods map[string]*v1.Pod)
	// AddUnschedulableIfNotPresent adds an unschedulable pod back to scheduling queue.
	// The podSchedulingCycle represents the current scheduling cycle number which can be
	// returned by calling SchedulingCycle().
	AddUnschedulableIfNotPresent(logger klog.Logger, pod *framework.QueuedPodInfo, podSchedulingCycle int64) error
	// SchedulingCycle returns the current number of scheduling cycle which is
	// cached by scheduling queue. Normally, incrementing this number whenever
	// a pod is popped (e.g. called Pop()) is enough.
	SchedulingCycle() int64
	// Pop removes the head of the queue and returns it. It blocks if the
	// queue is empty and waits until a new item is added to the queue.
	Pop(logger klog.Logger) (*framework.QueuedPodInfo, error)
	// Done must be called for pod returned by Pop. This allows the queue to
	// keep track of which pods are currently being processed.
	Done(types.UID)
	Update(logger klog.Logger, oldPod, newPod *v1.Pod)
	Delete(pod *v1.Pod)
	// Important Note: preCheck shouldn't include anything that depends on the in-tree plugins' logic.
	// (e.g., filter Pods based on added/updated Node's capacity, etc.)
	// We know currently some do, but we'll eventually remove them in favor of the scheduling queue hint.
	MoveAllToActiveOrBackoffQueue(logger klog.Logger, event framework.ClusterEvent, oldObj, newObj interface{}, preCheck PreEnqueueCheck)
	AssignedPodAdded(logger klog.Logger, pod *v1.Pod)
	AssignedPodUpdated(logger klog.Logger, oldPod, newPod *v1.Pod, event framework.ClusterEvent)

	// Close closes the SchedulingQueue so that the goroutine which is
	// waiting to pop items can exit gracefully.
	Close()
	// Run starts the goroutines managing the queue.
	Run(logger klog.Logger)

	// The following functions are supposed to be used only for testing or debugging.
	GetPod(name, namespace string) (*framework.QueuedPodInfo, bool)
	PendingPods() ([]*v1.Pod, string)
	InFlightPods() []*v1.Pod
	PodsInActiveQ() []*v1.Pod
	// PodsInBackoffQ returns all the Pods in the backoffQ.
	PodsInBackoffQ() []*v1.Pod
	UnschedulablePods() []*v1.Pod
}

// NewSchedulingQueue initializes a priority queue as a new scheduling queue.
func NewSchedulingQueue(
	lessFn framework.LessFunc,
	client kubernetes.Interface,
	opts ...Option) SchedulingQueue {
	return NewPriorityQueue(lessFn, client, opts...)
}

// PriorityQueue implements a scheduling queue.
// The head of PriorityQueue is the highest priority pending pod. This structure
// has two sub queues and a additional data structure, namely: activeQ,
// backoffQ and unschedulablePods.
//   - activeQ holds pods that are being considered for scheduling.
//   - backoffQ holds pods that moved from unschedulablePods and will move to
//     activeQ when their backoff periods complete.
//   - unschedulablePods holds pods that were already attempted for scheduling and
//     are currently determined to be unschedulable.
type PriorityQueue struct {
	*nominator

	stop  chan struct{}
	clock clock.WithTicker

	// lock takes precedence and should be taken first,
	// before any other locks in the queue (activeQueue.lock or backoffQueue.lock or nominator.nLock).
	// Correct locking order is: lock > activeQueue.lock > backoffQueue.lock > nominator.nLock.
	lock sync.RWMutex

	// the maximum time a pod can stay in the unschedulablePods.
	podMaxInUnschedulablePodsDuration time.Duration

	activeQ  activeQueuer
	backoffQ backoffQueuer
	// unschedulablePods holds pods that have been tried and determined unschedulable.
	unschedulablePods *UnschedulablePods
	// moveRequestCycle caches the sequence number of scheduling cycle when we
	// received a move request. Unschedulable pods in and before this scheduling
	// cycle will be put back to activeQueue if we were trying to schedule them
	// when we received move request.
	// TODO: this will be removed after SchedulingQueueHint goes to stable and the feature gate is removed.
	moveRequestCycle int64

	//nsLister listersv1.NamespaceLister
	client kubernetes.Interface

	metricsRecorder metrics.MetricAsyncRecorder
	// pluginMetricsSamplePercent is the percentage of plugin metrics to be sampled.
	pluginMetricsSamplePercent int
}

// QueueingHintFunction is the wrapper of QueueingHintFn that has PluginName.
type QueueingHintFunction struct {
	PluginName     string
	QueueingHintFn framework.QueueingHintFn
}

type priorityQueueOptions struct {
	clock                             clock.WithTicker
	podInitialBackoffDuration         time.Duration
	podMaxBackoffDuration             time.Duration
	podMaxInUnschedulablePodsDuration time.Duration
	podLister                         listersv1.PodLister
	metricsRecorder                   metrics.MetricAsyncRecorder
	pluginMetricsSamplePercent        int
	preEnqueuePluginMap               map[string][]framework.PreEnqueuePlugin
}

// Option configures a PriorityQueue
type Option func(*priorityQueueOptions)

// WithClock sets clock for PriorityQueue, the default clock is clock.RealClock.
func WithClock(clock clock.WithTicker) Option {
	return func(o *priorityQueueOptions) {
		o.clock = clock
	}
}

// WithPodInitialBackoffDuration sets pod initial backoff duration for PriorityQueue.
func WithPodInitialBackoffDuration(duration time.Duration) Option {
	return func(o *priorityQueueOptions) {
		o.podInitialBackoffDuration = duration
	}
}

// WithPodMaxBackoffDuration sets pod max backoff duration for PriorityQueue.
func WithPodMaxBackoffDuration(duration time.Duration) Option {
	return func(o *priorityQueueOptions) {
		o.podMaxBackoffDuration = duration
	}
}

// WithPodMaxInUnschedulablePodsDuration sets podMaxInUnschedulablePodsDuration for PriorityQueue.
func WithPodMaxInUnschedulablePodsDuration(duration time.Duration) Option {
	return func(o *priorityQueueOptions) {
		o.podMaxInUnschedulablePodsDuration = duration
	}
}

// WithPreEnqueuePluginMap sets preEnqueuePluginMap for PriorityQueue.
func WithPreEnqueuePluginMap(m map[string][]framework.PreEnqueuePlugin) Option {
	return func(o *priorityQueueOptions) {
		o.preEnqueuePluginMap = m
	}
}

// WithMetricsRecorder sets metrics recorder.
func WithMetricsRecorder(recorder metrics.MetricAsyncRecorder) Option {
	return func(o *priorityQueueOptions) {
		o.metricsRecorder = recorder
	}
}

// WithPluginMetricsSamplePercent sets the percentage of plugin metrics to be sampled.
func WithPluginMetricsSamplePercent(percent int) Option {
	return func(o *priorityQueueOptions) {
		o.pluginMetricsSamplePercent = percent
	}
}

var defaultPriorityQueueOptions = priorityQueueOptions{
	clock:                             clock.RealClock{},
	podInitialBackoffDuration:         DefaultPodInitialBackoffDuration,
	podMaxBackoffDuration:             DefaultPodMaxBackoffDuration,
	podMaxInUnschedulablePodsDuration: DefaultPodMaxInUnschedulablePodsDuration,
}

// Making sure that PriorityQueue implements SchedulingQueue.
var _ SchedulingQueue = &PriorityQueue{}

// newQueuedPodInfoForLookup builds a QueuedPodInfo object for a lookup in the queue.
func newQueuedPodInfoForLookup(pod *v1.Pod, plugins ...string) *framework.QueuedPodInfo {
	// Since this is only used for a lookup in the queue, we only need to set the Pod,
	// and so we avoid creating a full PodInfo, which is expensive to instantiate frequently.
	return &framework.QueuedPodInfo{
		PodInfo:              &framework.PodInfo{Pod: pod},
		UnschedulablePlugins: sets.New(plugins...),
	}
}

// NewPriorityQueue creates a PriorityQueue object.
func NewPriorityQueue(
	lessFn framework.LessFunc,
	client kubernetes.Interface,
	opts ...Option,
) *PriorityQueue {
	options := defaultPriorityQueueOptions
	for _, opt := range opts {
		opt(&options)
	}

	var useDynamo bool
	useDynamo = true

	// Build AWS config once and share it
	var awsCfg aws.Config
	if useDynamo {
		awsCfg = awsstore.BuildAWSConfig()
		klog.Infof("Using DynamoDB backend for scheduler queues")

		// Ensure the partition store table exists if using DynamoDB.
		ddb := dynamodb.NewFromConfig(awsCfg)
		if err := awsstore.EnsurePartitionStoreTable(context.Background(), ddb, awsstore.PartitionStoreTableName); err != nil {
			panic(fmt.Sprintf("Failed to ensure partition store table: %v", err))
		}
	}

	// Initialize the backoff queue with the backend.
	backoffQ := newBackoffQueueWithBackend(
		options.clock,
		options.podInitialBackoffDuration,
		options.podMaxBackoffDuration,
		useDynamo,
		awsCfg,
	)

	// Initialize the unschedulable pods store.
	unschedulablePods := newUnschedulablePods(
		awsCfg,
		useDynamo,
		metrics.NewUnschedulablePodsRecorder(),
		metrics.NewGatedPodsRecorder())

	pq := &PriorityQueue{
		clock:                             options.clock,
		stop:                              make(chan struct{}),
		podMaxInUnschedulablePodsDuration: options.podMaxInUnschedulablePodsDuration,
		backoffQ:                          backoffQ,
		unschedulablePods:                 unschedulablePods,
		//preEnqueuePluginMap:               options.preEnqueuePluginMap,
		//queueingHintMap:                   options.queueingHintMap,
		metricsRecorder:            options.metricsRecorder,
		pluginMetricsSamplePercent: options.pluginMetricsSamplePercent,
		moveRequestCycle:           -1,
	}

	// Initialize the active queue with the backend.
	pq.activeQ = newActiveQueueWithBackend(
		options.metricsRecorder,
		lessFn,
		useDynamo,
		awsCfg)

	pq.client = client
	pq.nominator = newPodNominator(client)

	return pq
}

// Run starts the goroutine to pump from backoffQ to activeQ
func (p *PriorityQueue) Run(logger klog.Logger) {
	go p.backoffQ.waitUntilAlignedWithOrderingWindow(func() {
		p.flushBackoffQCompleted(logger)
	}, p.stop)
	go wait.Until(func() {
		p.flushUnschedulablePodsLeftover(logger)
	}, 30*time.Second, p.stop)
}

func (p *PriorityQueue) runPreEnqueuePlugin(ctx context.Context, pl framework.PreEnqueuePlugin, pod *v1.Pod, shouldRecordMetric bool) *framework.Status {
	if !shouldRecordMetric {
		return pl.PreEnqueue(ctx, pod)
	}
	startTime := p.clock.Now()
	s := pl.PreEnqueue(ctx, pod)
	p.metricsRecorder.ObservePluginDurationAsync(preEnqueue, pl.Name(), s.Code().String(), p.clock.Since(startTime).Seconds())
	return s
}

// moveToActiveQ tries to add the pod to the active queue.
// If the pod doesn't pass PreEnqueue plugins, it gets added to unschedulablePods instead.
// It returns a boolean flag to indicate whether the pod is added successfully.
func (p *PriorityQueue) moveToActiveQ(logger klog.Logger, pInfo *framework.QueuedPodInfo, event string) bool {
	// Create a new context with a timeout for this specific operation.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	gatedBefore := pInfo.Gated

	added := false
	p.activeQ.underLock(func(unlockedActiveQ unlockedActiveQueuer) {
		if pInfo.Gated {
			// Add the Pod to unschedulablePods if it's not passing PreEnqueuePlugins.
			if unlockedActiveQ.has(pInfo) {
				return
			}
			if p.backoffQ.has(pInfo) {
				return
			}
			if p.unschedulablePods.get(pInfo.Pod) != nil {
				return
			}
			p.unschedulablePods.addOrUpdate(pInfo, event)
			logger.V(5).Info("Pod moved to an internal scheduling queue, because the pod is gated", "pod", klog.KObj(pInfo.Pod), "event", event, "queue", unschedulablePods)
			return
		}
		if pInfo.InitialAttemptTimestamp == nil {
			now := p.clock.Now()
			pInfo.InitialAttemptTimestamp = &now
		}

		unlockedActiveQ.add(pInfo, event)
		added = true

		p.unschedulablePods.delete(pInfo.Pod, gatedBefore)
		p.backoffQ.delete(pInfo)
		logger.V(5).Info("Pod moved to an internal scheduling queue", "pod", klog.KObj(pInfo.Pod), "event", event, "queue", activeQ)
		if event == framework.EventUnscheduledPodAdd.Label() || event == framework.EventUnscheduledPodUpdate.Label() {
			p.AddNominatedPod(ctx, logger, pInfo.PodInfo, nil)
		}
	})
	return added
}

// moveToBackoffQ tries to add the pod to the backoff queue.
// If SchedulerPopFromBackoffQ feature gate is enabled and the pod doesn't pass PreEnqueue plugins, it gets added to unschedulablePods instead.
// It returns a boolean flag to indicate whether the pod is added successfully.
func (p *PriorityQueue) moveToBackoffQ(logger klog.Logger, pInfo *framework.QueuedPodInfo, event string) bool {
	p.backoffQ.add(logger, pInfo, event)
	logger.V(5).Info("Pod moved to an internal scheduling queue", "pod", klog.KObj(pInfo.Pod), "event", event, "queue", backoffQ)
	return true
}

// Add adds a pod to the active queue. It should be called only when a new pod
// is added so there is no chance the pod is already in active/unschedulable/backoff queues
func (p *PriorityQueue) Add(logger klog.Logger, pod *v1.Pod) {
	p.lock.Lock()
	defer p.lock.Unlock()

	pInfo := p.newQueuedPodInfo(pod)
	if added := p.moveToActiveQ(logger, pInfo, framework.EventUnscheduledPodAdd.Label()); added {
		p.activeQ.broadcast()
	}
}

// Activate moves the given pods to activeQ.
// If a pod isn't found in unschedulablePods or backoffQ and it's in-flight,
// the wildcard event is registered so that the pod will be requeued when it comes back.
// But, if a pod isn't found in unschedulablePods or backoffQ and it's not in-flight (i.e., completely unknown pod),
// Activate would ignore the pod.
func (p *PriorityQueue) Activate(logger klog.Logger, pods map[string]*v1.Pod) {
	p.lock.Lock()
	defer p.lock.Unlock()

	activated := false
	for _, pod := range pods {
		if p.activate(logger, pod) {
			activated = true
			continue
		}

		// If this pod is in-flight, register the activation event (for when QHint is enabled) or update moveRequestCycle (for when QHints is disabled)
		// so that the pod will be requeued when it comes back.
		// Specifically in the in-tree plugins, this is for the scenario with the preemption plugin
		// where the async preemption API calls are all done or fail at some point before the Pod comes back to the queue.
		//p.activeQ.addEventsIfPodInFlight(nil, pod, []framework.ClusterEvent{framework.EventForceActivate})
		p.moveRequestCycle = p.activeQ.schedulingCycle()
	}

	if activated {
		p.activeQ.broadcast()
	}
}

func (p *PriorityQueue) activate(logger klog.Logger, pod *v1.Pod) bool {
	var pInfo *framework.QueuedPodInfo
	// Verify if the pod is present in unschedulablePods or backoffQ.
	if pInfo = p.unschedulablePods.get(pod); pInfo == nil {
		// If the pod doesn't belong to unschedulablePods or backoffQ, don't activate it.
		// The pod can be already in activeQ.
		var exists bool
		pInfo, exists = p.backoffQ.get(newQueuedPodInfoForLookup(pod))
		if !exists {
			return false
		}
		// Delete pod from the backoffQ now to make sure it won't be popped from the backoffQ
		// just before moving it to the activeQ
		if deleted := p.backoffQ.delete(pInfo); !deleted {
			// Pod was popped from the backoffQ in the meantime. Don't activate it.
			return false
		}
	}

	if pInfo == nil {
		// Redundant safe check. We shouldn't reach here.
		logger.Error(nil, "Internal error: cannot obtain pInfo")
		return false
	}

	return p.moveToActiveQ(logger, pInfo, framework.ForceActivate)
}

// SchedulingCycle returns current scheduling cycle.
func (p *PriorityQueue) SchedulingCycle() int64 {
	return p.activeQ.schedulingCycle()
}

// addUnschedulableIfNotPresentWithoutQueueingHint inserts a pod that cannot be scheduled into
// the queue, unless it is already in the queue. Normally, PriorityQueue puts
// unschedulable pods in `unschedulablePods`. But if there has been a recent move
// request, then the pod is put in `backoffQ`.
// TODO: This function is called only when p.isSchedulingQueueHintEnabled is false,
// and this will be removed after SchedulingQueueHint goes to stable and the feature gate is removed.
func (p *PriorityQueue) addUnschedulableWithoutQueueingHint(logger klog.Logger, pInfo *framework.QueuedPodInfo, podSchedulingCycle int64) error {
	pod := pInfo.Pod
	// Refresh the timestamp since the pod is re-added.
	pInfo.Timestamp = p.clock.Now()

	// When the queueing hint is enabled, they are used differently.
	// But, we use all of them as UnschedulablePlugins when the queueing hint isn't enabled so that we don't break the old behaviour.
	rejectorPlugins := pInfo.UnschedulablePlugins.Union(pInfo.PendingPlugins)

	// If a move request has been received, move it to the BackoffQ, otherwise move
	// it to unschedulablePods.
	for plugin := range rejectorPlugins {
		metrics.UnschedulableReason(plugin, pInfo.Pod.Spec.SchedulerName).Inc()
	}
	if p.moveRequestCycle >= podSchedulingCycle || len(rejectorPlugins) == 0 {
		// Two cases to move a Pod to the active/backoff queue:
		// - The Pod is rejected by some plugins, but a move request is received after this Pod's scheduling cycle is started.
		//   In this case, the received event may be make Pod schedulable and we should retry scheduling it.
		// - No unschedulable plugins are associated with this Pod,
		//   meaning something unusual (a temporal failure on kube-apiserver, etc) happened and this Pod gets moved back to the queue.
		//   In this case, we should retry scheduling it because this Pod may not be retried until the next flush.
		if added := p.moveToBackoffQ(logger, pInfo, framework.ScheduleAttemptFailure); added {
		}
	} else {
		p.unschedulablePods.addOrUpdate(pInfo, framework.ScheduleAttemptFailure)
		logger.V(5).Info("Pod moved to an internal scheduling queue", "pod", klog.KObj(pod), "event", framework.ScheduleAttemptFailure, "queue", unschedulablePods)
	}

	return nil
}

// AddUnschedulableIfNotPresent inserts a pod that cannot be scheduled into
// the queue, unless it is already in the queue. Normally, PriorityQueue puts
// unschedulable pods in `unschedulablePods`. But if there has been a recent move
// request, then the pod is put in `backoffQ`.
func (p *PriorityQueue) AddUnschedulableIfNotPresent(logger klog.Logger, pInfo *framework.QueuedPodInfo, podSchedulingCycle int64) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	// In any case, this Pod will be moved back to the queue and we should call Done.
	defer p.Done(pInfo.Pod.UID)

	pod := pInfo.Pod
	if p.unschedulablePods.get(pod) != nil {
		return fmt.Errorf("Pod %v is already present in unschedulable queue", klog.KObj(pod))
	}

	if p.activeQ.has(pInfo) {
		return fmt.Errorf("Pod %v is already present in the active queue", klog.KObj(pod))
	}
	if p.backoffQ.has(pInfo) {
		return fmt.Errorf("Pod %v is already present in the backoff queue", klog.KObj(pod))
	}

	return p.addUnschedulableWithoutQueueingHint(logger, pInfo, podSchedulingCycle)
}

// flushBackoffQCompleted Moves all pods from backoffQ which have completed backoff in to activeQ
func (p *PriorityQueue) flushBackoffQCompleted(logger klog.Logger) {
	p.lock.Lock()
	defer p.lock.Unlock()
	activated := false
	podsCompletedBackoff := p.backoffQ.popAllBackoffCompleted(logger)
	for _, pInfo := range podsCompletedBackoff {
		if added := p.moveToActiveQ(logger, pInfo, framework.BackoffComplete); added {
			activated = true
		}
	}
	if activated {
		p.activeQ.broadcast()
	}
}

// flushUnschedulablePodsLeftover moves pods which stay in unschedulablePods
// longer than podMaxInUnschedulablePodsDuration to backoffQ or activeQ.
func (p *PriorityQueue) flushUnschedulablePodsLeftover(logger klog.Logger) {
	p.lock.Lock()
	defer p.lock.Unlock()

	var podsToMove []*framework.QueuedPodInfo
	currentTime := p.clock.Now()
	infos, _ := p.unschedulablePods.listAll()
	for _, pInfo := range infos {
		lastScheduleTime := pInfo.Timestamp
		if currentTime.Sub(lastScheduleTime) > p.podMaxInUnschedulablePodsDuration {
			podsToMove = append(podsToMove, pInfo)
		}
	}

	if len(podsToMove) > 0 {
		p.movePodsToActiveOrBackoffQueue(logger, podsToMove, framework.EventUnschedulableTimeout, nil, nil)
	}
}

// Pop removes the head of the active queue and returns it. It blocks if the
// activeQ is empty and waits until a new item is added to the queue. It
// increments scheduling cycle when a pod is popped.
// Note: This method should NOT be locked by the p.lock at any moment,
// as it would lead to scheduling throughput degradation.
func (p *PriorityQueue) Pop(logger klog.Logger) (*framework.QueuedPodInfo, error) {
	return p.activeQ.pop(logger)
}

// Done must be called for pod returned by Pop. This allows the queue to
// keep track of which pods are currently being processed.
func (p *PriorityQueue) Done(pod types.UID) {
	return
}

func (p *PriorityQueue) InFlightPods() []*v1.Pod {
	return nil
}

// isPodUpdated checks if the pod is updated in a way that it may have become
// schedulable. It drops status of the pod and compares it with old version,
// except for pod.status.resourceClaimStatuses: changing that may have an
// effect on scheduling.
func isPodUpdated(oldPod, newPod *v1.Pod) bool {
	strip := func(pod *v1.Pod) *v1.Pod {
		p := pod.DeepCopy()
		p.ResourceVersion = ""
		p.Generation = 0
		p.Status = v1.PodStatus{
			ResourceClaimStatuses: pod.Status.ResourceClaimStatuses,
		}
		p.ManagedFields = nil
		p.Finalizers = nil
		return p
	}
	return !reflect.DeepEqual(strip(oldPod), strip(newPod))
}

// Update updates a pod in the active or backoff queue if present. Otherwise, it removes
// the item from the unschedulable queue if pod is updated in a way that it may
// become schedulable and adds the updated one to the active queue.
// If pod is not present in any of the queues, it is added to the active queue.
func (p *PriorityQueue) Update(logger klog.Logger, oldPod, newPod *v1.Pod) {
	p.lock.Lock()
	defer p.lock.Unlock()

	// Create a new context for this specific update operation.
	// This prevents the API call from blocking forever while the queue is locked.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if oldPod != nil {
		oldPodInfo := newQueuedPodInfoForLookup(oldPod)
		// If the pod is already in the active queue, just update it there.
		if pInfo := p.activeQ.update(newPod, oldPodInfo); pInfo != nil {
			p.UpdateNominatedPod(ctx, logger, oldPod, pInfo.PodInfo)
			return
		}

		// If the pod is in the backoff queue, update it there.
		if pInfo := p.backoffQ.update(newPod, oldPodInfo); pInfo != nil {
			p.UpdateNominatedPod(ctx, logger, oldPod, pInfo.PodInfo)
			return
		}
	}

	// If the pod is in the unschedulable queue, updating it may make it schedulable.
	if pInfo := p.unschedulablePods.get(newPod); pInfo != nil {
		_ = pInfo.Update(newPod)
		p.UpdateNominatedPod(ctx, logger, oldPod, pInfo.PodInfo)
		gated := pInfo.Gated
		if isPodUpdated(oldPod, newPod) {
			// Pod might have completed its backoff time while being in unschedulablePods,
			// so we should check isPodBackingoff before moving the pod to backoffQ.
			if p.backoffQ.isPodBackingoff(pInfo) {
				if added := p.moveToBackoffQ(logger, pInfo, framework.EventUnscheduledPodUpdate.Label()); added {
					p.unschedulablePods.delete(pInfo.Pod, gated)
				}
				return
			}

			if added := p.moveToActiveQ(logger, pInfo, framework.EventUnscheduledPodUpdate.Label()); added {
				p.activeQ.broadcast()
			}
			return
		}

		// Pod update didn't make it schedulable, keep it in the unschedulable queue.
		p.unschedulablePods.addOrUpdate(pInfo, framework.EventUnscheduledPodUpdate.Label())
		return
	}
	// If pod is not in any of the queues, we put it in the active queue.
	pInfo := p.newQueuedPodInfo(newPod)
	if added := p.moveToActiveQ(logger, pInfo, framework.EventUnscheduledPodUpdate.Label()); added {
		p.activeQ.broadcast()
	}
}

// Delete deletes the item from either of the two queues. It assumes the pod is
// only in one queue.
func (p *PriorityQueue) Delete(pod *v1.Pod) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.DeleteNominatedPodIfExists(pod)
	pInfo := newQueuedPodInfoForLookup(pod)
	if err := p.activeQ.delete(pInfo); err == nil {
		return
	}
	if deleted := p.backoffQ.delete(pInfo); deleted {
		return
	}
	if pInfo = p.unschedulablePods.get(pod); pInfo != nil {
		p.unschedulablePods.delete(pod, pInfo.Gated)
	}
}

// AssignedPodAdded is called when a bound pod is added. Creation of this pod
// may make pending pods with matching affinity terms schedulable.
func (p *PriorityQueue) AssignedPodAdded(logger klog.Logger, pod *v1.Pod) {
	p.lock.Lock()

	// Create a background context with a 5-second timeout for the API call.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	logger.V(4).Info("Assigned pod added", "pod", klog.KObj(pod))

	// Pre-filter Pods to move by getUnschedulablePodsWithCrossTopologyTerm
	// because Pod related events shouldn't make Pods that rejected by single-node scheduling requirement schedulable.
	p.movePodsToActiveOrBackoffQueue(logger, p.getUnschedulablePodsWithCrossTopologyTerm(ctx, logger, pod), framework.EventAssignedPodAdd, nil, pod)
	p.lock.Unlock()
}

// AssignedPodUpdated is called when a bound pod is updated. Change of labels
// may make pending pods with matching affinity terms schedulable.
func (p *PriorityQueue) AssignedPodUpdated(logger klog.Logger, oldPod, newPod *v1.Pod, event framework.ClusterEvent) {
	p.lock.Lock()

	// Create a background context with a 5-second timeout for the API call.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	logger.V(4).Info("Assigned pod updated", "oldPod", klog.KObj(oldPod), "newPod", klog.KObj(newPod))

	if (framework.ClusterEvent{Resource: framework.Pod, ActionType: framework.UpdatePodScaleDown}.Match(event)) {
		// In this case, we don't want to pre-filter Pods by getUnschedulablePodsWithCrossTopologyTerm
		// because Pod related events may make Pods that were rejected by NodeResourceFit schedulable.
		p.moveAllToActiveOrBackoffQueue(logger, event, oldPod, newPod, nil)
	} else {
		// Pre-filter Pods to move by getUnschedulablePodsWithCrossTopologyTerm
		// because Pod related events only make Pods rejected by cross topology term schedulable.
		p.movePodsToActiveOrBackoffQueue(logger, p.getUnschedulablePodsWithCrossTopologyTerm(ctx, logger, newPod), event, oldPod, newPod)
	}
	p.lock.Unlock()
}

// NOTE: this function assumes a lock has been acquired in the caller.
// moveAllToActiveOrBackoffQueue moves all pods from unschedulablePods to activeQ or backoffQ.
// This function adds all pods and then signals the condition variable to ensure that
// if Pop() is waiting for an item, it receives the signal after all the pods are in the
// queue and the head is the highest priority pod.
func (p *PriorityQueue) moveAllToActiveOrBackoffQueue(logger klog.Logger, event framework.ClusterEvent, oldObj, newObj interface{}, preCheck PreEnqueueCheck) {
	all, _ := p.unschedulablePods.listAll()
	unschedulablePods := make([]*framework.QueuedPodInfo, 0, len(all))
	for _, pInfo := range all {
		if preCheck == nil || preCheck(pInfo.Pod) {
			unschedulablePods = append(unschedulablePods, pInfo)
		}
	}
	p.movePodsToActiveOrBackoffQueue(logger, unschedulablePods, event, oldObj, newObj)
}

// MoveAllToActiveOrBackoffQueue moves all pods from unschedulablePods to activeQ or backoffQ.
// This function adds all pods and then signals the condition variable to ensure that
// if Pop() is waiting for an item, it receives the signal after all the pods are in the
// queue and the head is the highest priority pod.
func (p *PriorityQueue) MoveAllToActiveOrBackoffQueue(logger klog.Logger, event framework.ClusterEvent, oldObj, newObj interface{}, preCheck PreEnqueueCheck) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.moveAllToActiveOrBackoffQueue(logger, event, oldObj, newObj, preCheck)
}

// requeuePodViaQueueingHint tries to requeue Pod to activeQ, backoffQ or unschedulable pod pool based on schedulingHint.
// It returns the queue name Pod goes.
//
// NOTE: this function assumes lock has been acquired in caller
func (p *PriorityQueue) requeuePodViaQueueingHint(logger klog.Logger, pInfo *framework.QueuedPodInfo, event string) string {
	// If the pod is still backing off, move it to the backoffQ.
	if p.backoffQ.isPodBackingoff(pInfo) {
		if added := p.moveToBackoffQ(logger, pInfo, event); added {
			return backoffQ
		}
		return unschedulablePods
	}

	// If backoff is finished or wasn't needed, move to the activeQ.
	if added := p.moveToActiveQ(logger, pInfo, event); added {
		return activeQ
	}

	// This happens if the pod is gated by a PreEnqueue plugin.
	return unschedulablePods
}

// NOTE: this function assumes lock has been acquired in caller
func (p *PriorityQueue) movePodsToActiveOrBackoffQueue(logger klog.Logger, podInfoList []*framework.QueuedPodInfo, event framework.ClusterEvent, oldObj, newObj interface{}) {
	activated := false
	for _, pInfo := range podInfoList {
		// When handling events takes time, a scheduling throughput gets impacted negatively
		// because of a shared lock within PriorityQueue, which Pop() also requires.
		//
		// Scheduling-gated Pods never get schedulable with any events,
		// except the Pods themselves got updated, which isn't handled by movePodsToActiveOrBackoffQueue.
		// So, we can skip them early here so that they don't go through isPodWorthRequeuing,
		// which isn't fast enough to keep a sufficient scheduling throughput
		// when the number of scheduling-gated Pods in unschedulablePods is large.
		// https://github.com/kubernetes/kubernetes/issues/124384
		// This is a hotfix for this issue, which might be changed
		// once we have a better general solution for the shared lock issue.
		//
		// Note that we cannot skip all pInfo.Gated Pods here
		// because PreEnqueue plugins apart from the scheduling gate plugin may change the gating status
		// with these events.
		if pInfo.Gated && pInfo.UnschedulablePlugins.Has(names.SchedulingGates) {
			continue
		}

		p.unschedulablePods.delete(pInfo.Pod, pInfo.Gated)
		queue := p.requeuePodViaQueueingHint(logger, pInfo, event.Label())
		logger.V(4).Info("Pod moved to an internal scheduling queue", "pod", klog.KObj(pInfo.Pod), "event", event.Label(), "queue", queue)
		if queue == activeQ {
			activated = true
		}
	}

	p.moveRequestCycle = p.activeQ.schedulingCycle()

	if activated {
		p.activeQ.broadcast()
	}
}

// getUnschedulablePodsWithCrossTopologyTerm returns unschedulable pods which either of following conditions is met:
// - have any affinity term that matches "pod".
// - rejected by PodTopologySpread plugin.
// NOTE: this function assumes lock has been acquired in caller.
func (p *PriorityQueue) getUnschedulablePodsWithCrossTopologyTerm(ctx context.Context, logger klog.Logger, pod *v1.Pod) []*framework.QueuedPodInfo {
	//nsLabels := interpodaffinity.GetNamespaceLabelsSnapshot(logger, pod.Namespace, p.nsLister)

	// --- Start of new inline logic to get namespace labels ---
	var nsLabels labels.Set
	backoff := retry.DefaultBackoff
	backoff.Duration = 100 * time.Millisecond
	retryErr := retry.OnError(backoff, apierrors.IsNotFound, func() error {
		podNS, err := p.client.CoreV1().Namespaces().Get(ctx, pod.Namespace, metav1.GetOptions{})
		if err == nil {
			nsLabels = labels.Merge(podNS.Labels, nil)
		}
		return err
	})
	if retryErr != nil {
		logger.V(3).Info("getting namespace, assuming empty set of namespace labels", "namespace", pod.Namespace, "err", retryErr)
	}
	// --- End of new inline logic ---

	// --- Start of Fix ---
	// Use a map to collect unique pods to move, using the pod UID as the key.
	podsToMoveMap := make(map[types.UID]*framework.QueuedPodInfo)
	infos, _ := p.unschedulablePods.listAll()
	for _, pInfo := range infos {
		if pInfo.UnschedulablePlugins.Has(podtopologyspread.Name) && pod.Namespace == pInfo.Pod.Namespace {
			podsToMoveMap[pInfo.Pod.UID] = pInfo
			continue
		}
		for _, term := range pInfo.RequiredAffinityTerms {
			if term.Matches(pod, nsLabels) {
				podsToMoveMap[pInfo.Pod.UID] = pInfo
				break
			}
		}
	}

	// Convert the map of unique pods back into a slice.
	podsToMove := make([]*framework.QueuedPodInfo, 0, len(podsToMoveMap))
	for _, pInfo := range podsToMoveMap {
		podsToMove = append(podsToMove, pInfo)
	}
	// --- End of Fix ---

	return podsToMove
}

// PodsInActiveQ returns all the Pods in the activeQ.
func (p *PriorityQueue) PodsInActiveQ() []*v1.Pod {
	return p.activeQ.list()
}

// PodsInBackoffQ returns all the Pods in the backoffQ.
func (p *PriorityQueue) PodsInBackoffQ() []*v1.Pod {
	return p.backoffQ.list()
}

// UnschedulablePods returns all the pods in unschedulable state.
func (p *PriorityQueue) UnschedulablePods() []*v1.Pod {
	var result []*v1.Pod
	infos, _ := p.unschedulablePods.listAll()
	for _, pInfo := range infos {
		result = append(result, pInfo.Pod)
	}
	return result
}

var pendingPodsSummary = "activeQ:%v; backoffQ:%v; unschedulablePods:%v"

// GetPod searches for a pod in the activeQ, backoffQ, and unschedulablePods.
func (p *PriorityQueue) GetPod(name, namespace string) (pInfo *framework.QueuedPodInfo, ok bool) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	pInfoLookup := &framework.QueuedPodInfo{
		PodInfo: &framework.PodInfo{
			Pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
			},
		},
	}
	if pInfo, ok = p.backoffQ.get(pInfoLookup); ok {
		return pInfo, true
	}
	if pInfo = p.unschedulablePods.get(pInfoLookup.Pod); pInfo != nil {
		return pInfo, true
	}

	p.activeQ.underRLock(func(unlockedActiveQ unlockedActiveQueueReader) {
		pInfo, ok = unlockedActiveQ.get(pInfoLookup)
	})
	return
}

// PendingPods returns all the pending pods in the queue; accompanied by a debugging string
// recording showing the number of pods in each queue respectively.
// This function is used for debugging purposes in the scheduler cache dumper and comparer.
func (p *PriorityQueue) PendingPods() ([]*v1.Pod, string) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	result := p.PodsInActiveQ()
	activeQLen := len(result)
	backoffQPods := p.PodsInBackoffQ()
	backoffQLen := len(backoffQPods)
	result = append(result, backoffQPods...)
	unschedInfos, _ := p.unschedulablePods.listAll()
	for _, pInfo := range unschedInfos {
		result = append(result, pInfo.Pod)
	}
	unschedQLen := len(unschedInfos)
	return result, fmt.Sprintf(pendingPodsSummary, activeQLen, backoffQLen, unschedQLen)
}

// Note: this function assumes the caller locks both p.lock.RLock and p.activeQ.getLock().RLock.
func (p *PriorityQueue) nominatedPodToInfo(np awsstore.PodRef, unlockedActiveQ unlockedActiveQueueReader) *framework.PodInfo {
	pod := np.ToPod()
	pInfoLookup := newQueuedPodInfoForLookup(pod)

	queuedPodInfo, exists := unlockedActiveQ.get(pInfoLookup)
	if exists {
		return queuedPodInfo.PodInfo
	}

	queuedPodInfo = p.unschedulablePods.get(pod)
	if queuedPodInfo != nil {
		return queuedPodInfo.PodInfo
	}

	queuedPodInfo, exists = p.backoffQ.get(pInfoLookup)
	if exists {
		return queuedPodInfo.PodInfo
	}

	return &framework.PodInfo{Pod: pod}
}

// Close closes the priority queue.
func (p *PriorityQueue) Close() {
	p.lock.Lock()
	defer p.lock.Unlock()
	close(p.stop)
	p.activeQ.close()
	p.activeQ.broadcast()
}

// NominatedPodsForNode returns a copy of pods that are nominated to run on the given node,
// but they are waiting for other pods to be removed from the node.
// CAUTION: Make sure you don't call this function while taking any queue's lock in any scenario.
func (p *PriorityQueue) NominatedPodsForNode(nodeName string) []*framework.PodInfo {
	p.lock.RLock()
	defer p.lock.RUnlock()
	nominatedPods := p.nominator.nominatedPodsForNode(nodeName)

	pods := make([]*framework.PodInfo, len(nominatedPods))
	p.activeQ.underRLock(func(unlockedActiveQ unlockedActiveQueueReader) {
		for i, np := range nominatedPods {
			pods[i] = p.nominatedPodToInfo(np, unlockedActiveQ).DeepCopy()
		}
	})
	return pods
}

// newQueuedPodInfo builds a QueuedPodInfo object.
func (p *PriorityQueue) newQueuedPodInfo(pod *v1.Pod, plugins ...string) *framework.QueuedPodInfo {
	now := p.clock.Now()
	// ignore this err since apiserver doesn't properly validate affinity terms
	// and we can't fix the validation for backwards compatibility.
	podInfo, _ := framework.NewPodInfo(pod)
	return &framework.QueuedPodInfo{
		PodInfo:                 podInfo,
		Timestamp:               now,
		InitialAttemptTimestamp: nil,
		UnschedulablePlugins:    sets.New(plugins...),
	}
}

// UnschedulablePods holds pods that cannot be scheduled. This data structure
// is used to implement unschedulablePods.
type UnschedulablePods struct {
	// podInfoMap is a map key by a pod's full-name and the value is a pointer to the QueuedPodInfo.
	//podInfoMap map[string]*framework.QueuedPodInfo
	store   awsstore.PodStore
	keyFunc func(*v1.Pod) string
	// unschedulableRecorder/gatedRecorder updates the counter when elements of an unschedulablePodsMap
	// get added or removed, and it does nothing if it's nil.
	unschedulableRecorder, gatedRecorder metrics.MetricRecorder
}

// addOrUpdate adds a pod to the unschedulable podInfoMap.
// The event should show which event triggered the addition and is used for the metric recording.
func (u *UnschedulablePods) addOrUpdate(pInfo *framework.QueuedPodInfo, event string) {
	podID := util.GetPodFullName(pInfo.Pod)
	if _, exists, _ := u.store.Get(podID); !exists {
		if pInfo.Gated && u.gatedRecorder != nil {
			u.gatedRecorder.Inc()
		} else if !pInfo.Gated && u.unschedulableRecorder != nil {
			u.unschedulableRecorder.Inc()
		}
		metrics.SchedulerQueueIncomingPods.WithLabelValues("unschedulable", event).Inc()
	}
	_ = u.store.AddOrUpdate(podID, pInfo)
}

// delete deletes a pod from the unschedulable podInfoMap.
// The `gated` parameter is used to figure out which metric should be decreased.
func (u *UnschedulablePods) delete(pod *v1.Pod, gated bool) {
	podID := util.GetPodFullName(pod)
	if _, exists, _ := u.store.Get(podID); exists {
		if gated && u.gatedRecorder != nil {
			u.gatedRecorder.Dec()
		} else if !gated && u.unschedulableRecorder != nil {
			u.unschedulableRecorder.Dec()
		}
	}
	_ = u.store.Delete(podID)
}

// get returns the QueuedPodInfo if a pod with the same key as the key of the given "pod"
// is found in the map. It returns nil otherwise.
func (u *UnschedulablePods) get(pod *v1.Pod) *framework.QueuedPodInfo {
	podKey := util.GetPodFullName(pod)
	if pInfo, ok, _ := u.store.Get(podKey); ok {
		return pInfo
	}
	return nil
}

// clear removes all the entries from the unschedulable podInfoMap.
func (u *UnschedulablePods) clear() {
	_ = u.store.Clear()
	if u.unschedulableRecorder != nil {
		u.unschedulableRecorder.Clear()
	}
	if u.gatedRecorder != nil {
		u.gatedRecorder.Clear()
	}
}

func (u *UnschedulablePods) listAll() ([]*framework.QueuedPodInfo, error) {
	return u.store.List()
}

// newUnschedulablePods initializes a new object of UnschedulablePods.
func newUnschedulablePods(awsCfg aws.Config, useDynamo bool, unschedulableRecorder, gatedRecorder metrics.MetricRecorder) *UnschedulablePods {

	var store awsstore.PodStore
	if useDynamo {
		ddb := dynamodb.NewFromConfig(awsCfg)

		ps, err := awsstore.NewPartitionStore(
			context.Background(),
			ddb,
			awsstore.PartitionStoreTableName,
			"unschedulable-pods",
			0,
			true,
		)
		if err != nil {
			panic(err)
		}
		store = awsstore.NewDDBPodStore(context.Background(), ps)
		klog.Infof("Using DynamoDB backend for unschedulable pods")
	} else {
		store = awsstore.NewMapStore()
		klog.Infof("Using in-memory backend for unschedulable pods")
	}

	return &UnschedulablePods{
		store:                 store,
		unschedulableRecorder: unschedulableRecorder,
		gatedRecorder:         gatedRecorder,
	}
}

func podInfoKeyFunc(pInfo *framework.QueuedPodInfo) string {
	return cache.NewObjectName(pInfo.Pod.Namespace, pInfo.Pod.Name).String()
}
