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

package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	configv1 "k8s.io/kube-scheduler/config/v1"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/scheme"
	internalcache "k8s.io/kubernetes/pkg/scheduler/backend/cache"
	internalqueue "k8s.io/kubernetes/pkg/scheduler/backend/queue"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/parallelize"
	frameworkplugins "k8s.io/kubernetes/pkg/scheduler/framework/plugins"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	"k8s.io/kubernetes/pkg/scheduler/profile"
	"k8s.io/utils/clock"
)

const (
	// Duration the scheduler will wait before expiring an assumed pod.
	// See issue #106361 for more details about this parameter and its value.
	durationToExpireAssumedPod time.Duration = 0
)

// ErrNoNodesAvailable is used to describe the error that no nodes available to schedule pods.
var ErrNoNodesAvailable = fmt.Errorf("no nodes available to schedule pods")

// Scheduler watches for new unscheduled pods. It attempts to find
// nodes that they fit on and writes bindings back to the api server.
type Scheduler struct {
	// It is expected that changes made via Cache will be observed
	// by NodeLister and Algorithm.
	Cache internalcache.Cache

	// NextPod should be a function that blocks until the next pod
	// is available. We don't use a channel for this, because scheduling
	// a pod may take some amount of time and we don't want pods to get
	// stale while they sit in a channel.
	NextPod func(logger klog.Logger) (*framework.QueuedPodInfo, error)

	// FailureHandler is called upon a scheduling failure.
	FailureHandler FailureHandlerFn

	// SchedulePod tries to schedule the given pod to one of the nodes in the node list.
	// Return a struct of ScheduleResult with the name of suggested host on success,
	// otherwise will return a FitError with reasons.
	SchedulePod func(ctx context.Context, fwk framework.Framework, state *framework.CycleState, pod *v1.Pod) (ScheduleResult, error)

	// Close this to shut down the scheduler.
	StopEverything <-chan struct{}

	// SchedulingQueue holds pods to be scheduled
	SchedulingQueue internalqueue.SchedulingQueue

	// Profiles are the scheduling profiles.
	//Profiles profile.Map
	Framework framework.Framework

	client clientset.Interface

	nodeInfoSnapshot *internalcache.Snapshot

	percentageOfNodesToScore int32

	nextStartNodeIndex int

	// logger *must* be initialized when creating a Scheduler,
	// otherwise logging functions will access a nil sink and
	// panic.
	logger klog.Logger
}

func (sched *Scheduler) applyDefaultHandlers() {
	sched.SchedulePod = sched.schedulePod
	sched.FailureHandler = sched.handleSchedulingFailure
}

type schedulerOptions struct {
	clock                  clock.WithTicker
	componentConfigVersion string
	kubeConfig             *restclient.Config
	// Overridden by profile level percentageOfNodesToScore if set in v1.
	percentageOfNodesToScore          int32
	podInitialBackoffSeconds          int64
	podMaxBackoffSeconds              int64
	podMaxInUnschedulablePodsDuration time.Duration
	// Contains out-of-tree plugins to be merged with the in-tree registry.
	frameworkOutOfTreeRegistry frameworkruntime.Registry
	profiles                   []schedulerapi.KubeSchedulerProfile
	frameworkCapturer          FrameworkCapturer
	parallelism                int32
	applyDefaultProfile        bool
}

// Option configures a Scheduler
type Option func(*schedulerOptions)

// ScheduleResult represents the result of scheduling a pod.
type ScheduleResult struct {
	// Name of the selected node.
	SuggestedHost string
	// The number of nodes the scheduler evaluated the pod against in the filtering
	// phase and beyond.
	EvaluatedNodes int
	// The number of nodes out of the evaluated ones that fit the pod.
	FeasibleNodes int
	// The nominating info for scheduling cycle.
	NominatingInfo *framework.NominatingInfo
}

// WithComponentConfigVersion sets the component config version to the
// KubeSchedulerConfiguration version used. The string should be the full
// scheme group/version of the external type we converted from (for example
// "kubescheduler.config.k8s.io/v1")
func WithComponentConfigVersion(apiVersion string) Option {
	return func(o *schedulerOptions) {
		o.componentConfigVersion = apiVersion
	}
}

// WithKubeConfig sets the kube config for Scheduler.
func WithKubeConfig(cfg *restclient.Config) Option {
	return func(o *schedulerOptions) {
		o.kubeConfig = cfg
	}
}

// WithProfiles sets profiles for Scheduler. By default, there is one profile
// with the name "default-scheduler".
func WithProfiles(p ...schedulerapi.KubeSchedulerProfile) Option {
	return func(o *schedulerOptions) {
		o.profiles = p
		o.applyDefaultProfile = false
	}
}

// WithParallelism sets the parallelism for all scheduler algorithms. Default is 16.
func WithParallelism(threads int32) Option {
	return func(o *schedulerOptions) {
		o.parallelism = threads
	}
}

// WithPercentageOfNodesToScore sets percentageOfNodesToScore for Scheduler.
// The default value of 0 will use an adaptive percentage: 50 - (num of nodes)/125.
func WithPercentageOfNodesToScore(percentageOfNodesToScore *int32) Option {
	return func(o *schedulerOptions) {
		if percentageOfNodesToScore != nil {
			o.percentageOfNodesToScore = *percentageOfNodesToScore
		}
	}
}

// WithFrameworkOutOfTreeRegistry sets the registry for out-of-tree plugins. Those plugins
// will be appended to the default registry.
func WithFrameworkOutOfTreeRegistry(registry frameworkruntime.Registry) Option {
	return func(o *schedulerOptions) {
		o.frameworkOutOfTreeRegistry = registry
	}
}

// WithPodInitialBackoffSeconds sets podInitialBackoffSeconds for Scheduler, the default value is 1
func WithPodInitialBackoffSeconds(podInitialBackoffSeconds int64) Option {
	return func(o *schedulerOptions) {
		o.podInitialBackoffSeconds = podInitialBackoffSeconds
	}
}

// WithPodMaxBackoffSeconds sets podMaxBackoffSeconds for Scheduler, the default value is 10
func WithPodMaxBackoffSeconds(podMaxBackoffSeconds int64) Option {
	return func(o *schedulerOptions) {
		o.podMaxBackoffSeconds = podMaxBackoffSeconds
	}
}

// WithPodMaxInUnschedulablePodsDuration sets podMaxInUnschedulablePodsDuration for PriorityQueue.
func WithPodMaxInUnschedulablePodsDuration(duration time.Duration) Option {
	return func(o *schedulerOptions) {
		o.podMaxInUnschedulablePodsDuration = duration
	}
}

// WithClock sets clock for PriorityQueue, the default clock is clock.RealClock.
func WithClock(clock clock.WithTicker) Option {
	return func(o *schedulerOptions) {
		o.clock = clock
	}
}

// FrameworkCapturer is used for registering a notify function in building framework.
type FrameworkCapturer func(schedulerapi.KubeSchedulerProfile)

// WithBuildFrameworkCapturer sets a notify function for getting buildFramework details.
func WithBuildFrameworkCapturer(fc FrameworkCapturer) Option {
	return func(o *schedulerOptions) {
		o.frameworkCapturer = fc
	}
}

var defaultSchedulerOptions = schedulerOptions{
	clock:                             clock.RealClock{},
	percentageOfNodesToScore:          schedulerapi.DefaultPercentageOfNodesToScore,
	podInitialBackoffSeconds:          int64(internalqueue.DefaultPodInitialBackoffDuration.Seconds()),
	podMaxBackoffSeconds:              int64(internalqueue.DefaultPodMaxBackoffDuration.Seconds()),
	podMaxInUnschedulablePodsDuration: internalqueue.DefaultPodMaxInUnschedulablePodsDuration,
	parallelism:                       int32(parallelize.DefaultParallelism),
	// Ideally we would statically set the default profile here, but we can't because
	// creating the default profile may require testing feature gates, which may get
	// set dynamically in tests. Therefore, we delay creating it until New is actually
	// invoked.
	applyDefaultProfile: true,
}

// New returns a Scheduler
func New(ctx context.Context,
	client clientset.Interface,
	recorderFactory profile.RecorderFactory,
	opts ...Option) (*Scheduler, error) {

	logger := klog.FromContext(ctx)
	stopEverything := ctx.Done()

	options := defaultSchedulerOptions
	for _, opt := range opts {
		opt(&options)
	}

	if options.applyDefaultProfile {
		var versionedCfg configv1.KubeSchedulerConfiguration
		scheme.Scheme.Default(&versionedCfg)
		cfg := schedulerapi.KubeSchedulerConfiguration{}
		if err := scheme.Scheme.Convert(&versionedCfg, &cfg, nil); err != nil {
			return nil, err
		}
		options.profiles = cfg.Profiles
	}

	registry := frameworkplugins.NewInTreeRegistry()

	metrics.Register()

	schedulerCache := internalcache.New(ctx, durationToExpireAssumedPod)
	snap := schedulerCache.Snapshot()

	metricsRecorder := metrics.NewMetricsAsyncRecorder(1000, time.Second, stopEverything)
	// waitingPods holds all the pods that are in the scheduler and waiting in the permit stage
	waitingPods := frameworkruntime.NewWaitingPodsMap()

	profiles, err := profile.NewMap(ctx, options.profiles, registry, recorderFactory,
		frameworkruntime.WithComponentConfigVersion(options.componentConfigVersion),
		frameworkruntime.WithClientSet(client),
		frameworkruntime.WithKubeConfig(options.kubeConfig),
		frameworkruntime.WithSnapshotSharedLister(snap),
		frameworkruntime.WithCaptureProfile(frameworkruntime.CaptureProfile(options.frameworkCapturer)),
		frameworkruntime.WithParallelism(int(options.parallelism)),
		frameworkruntime.WithMetricsRecorder(metricsRecorder),
		frameworkruntime.WithWaitingPods(waitingPods),
	)
	if err != nil {
		return nil, fmt.Errorf("initializing profiles: %v", err)
	}

	if len(profiles) == 0 {
		return nil, errors.New("at least one profile is required")
	}

	podQueue := internalqueue.NewSchedulingQueue(
		profiles[options.profiles[0].SchedulerName].QueueSortFunc(),
		client,
		internalqueue.WithClock(options.clock),
		internalqueue.WithPodInitialBackoffDuration(time.Duration(options.podInitialBackoffSeconds)*time.Second),
		internalqueue.WithPodMaxBackoffDuration(time.Duration(options.podMaxBackoffSeconds)*time.Second),
		internalqueue.WithPodMaxInUnschedulablePodsDuration(options.podMaxInUnschedulablePodsDuration),
		internalqueue.WithPluginMetricsSamplePercent(pluginMetricsSamplePercent),
		internalqueue.WithMetricsRecorder(*metricsRecorder),
	)

	for _, fwk := range profiles {
		fwk.SetPodNominator(podQueue)
		fwk.SetPodActivator(podQueue)
	}

	defaultFwk := profiles[options.profiles[0].SchedulerName]

	sched := &Scheduler{
		Cache:                    schedulerCache,
		client:                   client,
		nodeInfoSnapshot:         snap,
		percentageOfNodesToScore: options.percentageOfNodesToScore,
		StopEverything:           stopEverything,
		SchedulingQueue:          podQueue,
		//Profiles:                 profiles,
		Framework: defaultFwk,
		logger:    logger,
	}
	sched.NextPod = podQueue.Pop
	sched.applyDefaultHandlers()

	return sched, nil
}

// Run begins watching and scheduling. It starts scheduling and blocked until the context is done.
func (sched *Scheduler) Run(ctx context.Context) {
	logger := klog.FromContext(ctx)
	//sched.SchedulingQueue.Run(logger)

	//sched.pollingLoop(ctx)

	// Start the new polling loop in the background.
	//go wait.UntilWithContext(ctx, sched.pollingLoop, 5*time.Second)

	// We need to start scheduleOne loop in a dedicated goroutine,
	// because scheduleOne function hangs on getting the next item
	// from the SchedulingQueue.
	// If there are no new pods to schedule, it will be hanging there
	// and if done in this goroutine it will be blocking closing
	// SchedulingQueue, in effect causing a deadlock on shutdown.
	//go wait.UntilWithContext(ctx, sched.ScheduleOne, 0)

	<-ctx.Done()
	sched.SchedulingQueue.Close()

	// If the plugins satisfy the io.Closer interface, they are closed.
	//err := sched.Profiles.Close()
	err := sched.Framework.Close()
	if err != nil {
		logger.Error(err, "Failed to close plugins")
	}
}

func (sched *Scheduler) pollingLoop(ctx context.Context) {
	logger := klog.FromContext(ctx)
	logger.V(4).Info("Starting cache reconciliation poll")

	// --- 1. Fetch Current State from API Server ---
	nodeList, err := sched.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		logger.Error(err, "Polling failed: cannot list nodes")
		return
	}
	scheduledPodsList, err := sched.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermNotEqualSelector("spec.nodeName", "").String(),
	})
	if err != nil {
		logger.Error(err, "Polling failed: cannot list scheduled pods")
		return
	}
	unscheduledPodsList, err := sched.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", "").String(),
	})
	if err != nil {
		logger.Error(err, "Polling failed: cannot list unscheduled pods")
		return
	}

	// --- 2. Get Current State from Local Cache ---
	if err := sched.Cache.UpdateSnapshot(logger, sched.nodeInfoSnapshot); err != nil {
		logger.Error(err, "Polling failed: cannot update snapshot")
		return
	}
	cachedNodeInfos, err := sched.nodeInfoSnapshot.NodeInfos().List()
	if err != nil {
		logger.Error(err, "Polling failed: cannot list nodes from snapshot")
		return
	}

	// --- 3. Reconcile Nodes ---
	apiNodesMap := make(map[string]*v1.Node)
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		apiNodesMap[node.Name] = node
	}
	cachedNodesMap := make(map[string]*v1.Node)
	for _, nodeInfo := range cachedNodeInfos {
		cachedNodesMap[nodeInfo.Node().Name] = nodeInfo.Node()
	}

	// Handle node additions and updates
	for name, apiNode := range apiNodesMap {
		if cachedNode, exists := cachedNodesMap[name]; !exists {
			sched.Cache.AddNode(logger, apiNode)
		} else if apiNode.ResourceVersion != cachedNode.ResourceVersion {
			sched.Cache.UpdateNode(logger, cachedNode, apiNode)
		}
	}
	// Handle node deletions
	for name, cachedNode := range cachedNodesMap {
		if _, exists := apiNodesMap[name]; !exists {
			err := sched.Cache.RemoveNode(logger, cachedNode)
			if err != nil {
				logger.Error(err, "Polling failed: cannot remove node", "node", klog.KObj(cachedNode))
				return
			}
		}
	}

	// --- 4. Reconcile Pods ---
	apiPodsMap := make(map[types.UID]*v1.Pod)
	for i := range scheduledPodsList.Items {
		pod := &scheduledPodsList.Items[i]
		apiPodsMap[pod.UID] = pod
	}
	cachedPodsMap := make(map[types.UID]*v1.Pod)
	for _, nodeInfo := range cachedNodeInfos {
		for _, podInfo := range nodeInfo.Pods {
			cachedPodsMap[podInfo.Pod.UID] = podInfo.Pod
		}
	}

	// Handle scheduled pod additions and updates
	for uid, apiPod := range apiPodsMap {
		if cachedPod, exists := cachedPodsMap[uid]; !exists {
			// Pod is in API but not cache -> ADD
			err := sched.Cache.AddPod(logger, apiPod)
			if err != nil {
				logger.Error(err, "Polling failed: cannot add pod", "pod", klog.KObj(apiPod))
				return
			}
		} else {
			// Pod is in both -> check if it needs an UPDATE
			if apiPod.ResourceVersion != cachedPod.ResourceVersion {
				err := sched.Cache.UpdatePod(logger, cachedPod, apiPod)
				if err != nil {
					logger.Error(err, "Polling failed: cannot update pod", "pod", klog.KObj(apiPod))
					return
				}
			}
		}
	}
	// Handle scheduled pod deletions
	for uid, cachedPod := range cachedPodsMap {
		if _, exists := apiPodsMap[uid]; !exists {
			// Pod is in cache but not API -> DELETE
			err := sched.Cache.RemovePod(logger, cachedPod)
			if err != nil {
				logger.Error(err, "Polling failed: cannot remove pod", "pod", klog.KObj(cachedPod))
				return
			}
		}
	}

	// --- 5. Reconcile Scheduling Queue ---
	pendingPods, _ := sched.SchedulingQueue.PendingPods()
	apiUnscheduledPodsMap := make(map[types.UID]*v1.Pod)
	for i := range unscheduledPodsList.Items {
		pod := &unscheduledPodsList.Items[i]
		apiUnscheduledPodsMap[pod.UID] = pod
	}
	queuePodsMap := make(map[types.UID]*v1.Pod)
	for _, pod := range pendingPods {
		queuePodsMap[pod.UID] = pod
	}
	// Handle Adds and Updates for unscheduled pods
	for uid, apiPod := range apiUnscheduledPodsMap {
		if queuePod, exists := queuePodsMap[uid]; !exists {
			sched.SchedulingQueue.Add(logger, apiPod)
		} else if apiPod.ResourceVersion != queuePod.ResourceVersion {
			sched.SchedulingQueue.Update(logger, queuePod, apiPod)
		}
	}
	// Handle Deletes for unscheduled pods
	for uid, queuePod := range queuePodsMap {
		if _, exists := apiUnscheduledPodsMap[uid]; !exists {
			sched.SchedulingQueue.Delete(queuePod)
		}
	}

	// --- 6. Periodically Re-evaluate Unschedulable Pods ---
	event := framework.ClusterEvent{Resource: framework.WildCard, ActionType: framework.All}
	sched.SchedulingQueue.MoveAllToActiveOrBackoffQueue(logger, event, nil, nil, nil)

	logger.V(4).Info("Cache reconciliation poll completed")
}

// NewInformerFactory creates a SharedInformerFactory and initializes a scheduler specific
// in-place podInformer.
func NewInformerFactory(cs clientset.Interface, resyncPeriod time.Duration) informers.SharedInformerFactory {
	informerFactory := informers.NewSharedInformerFactory(cs, resyncPeriod)
	informerFactory.InformerFor(&v1.Pod{}, newPodInformer)
	return informerFactory
}

type FailureHandlerFn func(ctx context.Context, fwk framework.Framework, podInfo *framework.QueuedPodInfo, status *framework.Status, nominatingInfo *framework.NominatingInfo, start time.Time)

// newPodInformer creates a shared index informer that returns only non-terminal pods.
// The PodInformer allows indexers to be added, but note that only non-conflict indexers are allowed.
func newPodInformer(cs clientset.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	selector := fmt.Sprintf("status.phase!=%v,status.phase!=%v", v1.PodSucceeded, v1.PodFailed)
	tweakListOptions := func(options *metav1.ListOptions) {
		options.FieldSelector = selector
	}
	informer := coreinformers.NewFilteredPodInformer(cs, metav1.NamespaceAll, resyncPeriod, cache.Indexers{}, tweakListOptions)

	// Dropping `.metadata.managedFields` to improve memory usage.
	// The Extract workflow (i.e. `ExtractPod`) should be unused.
	trim := func(obj interface{}) (interface{}, error) {
		if accessor, err := meta.Accessor(obj); err == nil {
			if accessor.GetManagedFields() != nil {
				accessor.SetManagedFields(nil)
			}
		}
		return obj, nil
	}
	informer.SetTransform(trim)
	return informer
}
