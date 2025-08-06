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

package cache

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"k8s.io/kubernetes/pkg/scheduler/awsstore"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

var (
	cleanAssumedPeriod = 1 * time.Second
)

// New returns a Cache implementation.
// It automatically starts a go routine that manages expiration of assumed pods.
// "ttl" is how long the assumed pod will get expired.
// "ctx" is the context that would close the background goroutine.
func New(ctx context.Context, ttl time.Duration) Cache {
	logger := klog.FromContext(ctx)
	cache := newCache(ctx, ttl, cleanAssumedPeriod)
	cache.run(logger)
	return cache
}

// nodeInfoListItem holds a NodeInfo pointer and acts as an item in a doubly
// linked list. When a NodeInfo is updated, it goes to the head of the list.
// The items closer to the head are the most recently updated items.
type nodeInfoListItem struct {
	info *framework.NodeInfo
	next *nodeInfoListItem
	prev *nodeInfoListItem
}

type cacheImpl struct {
	stop   <-chan struct{}
	ttl    time.Duration
	period time.Duration

	// This mutex guards all fields within this cache struct.
	mu sync.RWMutex
	//nodes map[string]*nodeInfoListItem
	// headNode points to the most recently updated NodeInfo in "nodes". It is the
	// head of the linked list.
	//headNode      *nodeInfoListItem
	//nodeTree      *nodeTree
	podStateStore awsstore.PodStateStore
	nodeStore     awsstore.NodeStore

	snapshot *Snapshot
}

func newCache(ctx context.Context, ttl, period time.Duration) *cacheImpl {
	useDynamo := true

	var (
		awsCfg aws.Config
		pss    awsstore.PodStateStore
		ns     awsstore.NodeStore
	)

	if useDynamo {
		awsCfg = awsstore.BuildAWSConfig()
		klog.Infof("Using DynamoDB backend for scheduler cache")

		ddb := dynamodb.NewFromConfig(awsCfg)

		// Partition store table (already ensured elsewhere for pod-state)
		if err := awsstore.EnsurePartitionStoreTable(ctx, ddb, awsstore.PartitionStoreTableName); err != nil {
			panic(fmt.Sprintf("Failed to ensure partition store table: %v", err))
		}

		// Pod state
		ps, err := awsstore.NewPartitionStore(ctx, ddb, awsstore.PartitionStoreTableName, "cache-podstate", 0, true)
		if err != nil {
			panic(err)
		}
		pss = awsstore.NewDDBPodStateStore(ctx, ps)

		// Node store table
		if err := awsstore.EnsureNodeStoreTable(ctx, ddb, awsstore.NodeStoreTableName); err != nil {
			panic(fmt.Sprintf("Failed to ensure node store table: %v", err))
		}
		nsAdapter, err := awsstore.NewDDBNodeStore(ctx, ddb, awsstore.NodeStoreTableName, true /*wipe on start for dev*/)
		if err != nil {
			panic(err)
		}
		ns = nsAdapter

		klog.Infof("Using DynamoDB backend for node store")
	} else {
		klog.Infof("Using in-memory backend for scheduler cache")
		pss = awsstore.NewInMemoryPodStateStore()
		ns = awsstore.NewMemNodeStore()
	}

	snap := NewEmptySnapshot(ns)

	return &cacheImpl{
		ttl:    ttl,
		period: period,
		stop:   ctx.Done(),

		//nodes:    make(map[string]*nodeInfoListItem),
		//nodeTree:      newNodeTree(logger, nil),
		podStateStore: pss,
		nodeStore:     ns,
		snapshot:      snap,
	}
}

// newNodeInfoListItem initializes a new nodeInfoListItem.
func newNodeInfoListItem(ni *framework.NodeInfo) *nodeInfoListItem {
	return &nodeInfoListItem{
		info: ni,
	}
}

//// moveNodeInfoToHead moves a NodeInfo to the head of "cache.nodes" doubly
//// linked list. The head is the most recently updated NodeInfo.
//// We assume cache lock is already acquired.
//func (cache *cacheImpl) moveNodeInfoToHead(logger klog.Logger, name string) {
//	ni, ok := cache.nodes[name]
//	if !ok {
//		logger.Error(nil, "No node info with given name found in the cache", "node", klog.KRef("", name))
//		return
//	}
//	// if the node info list item is already at the head, we are done.
//	if ni == cache.headNode {
//		return
//	}
//
//	if ni.prev != nil {
//		ni.prev.next = ni.next
//	}
//	if ni.next != nil {
//		ni.next.prev = ni.prev
//	}
//	if cache.headNode != nil {
//		cache.headNode.prev = ni
//	}
//	ni.next = cache.headNode
//	ni.prev = nil
//	cache.headNode = ni
//}

//// removeNodeInfoFromList removes a NodeInfo from the "cache.nodes" doubly
//// linked list.
//// We assume cache lock is already acquired.
//func (cache *cacheImpl) removeNodeInfoFromList(logger klog.Logger, name string) {
//	ni, ok := cache.nodes[name]
//	if !ok {
//		logger.Error(nil, "No node info with given name found in the cache", "node", klog.KRef("", name))
//		return
//	}
//
//	if ni.prev != nil {
//		ni.prev.next = ni.next
//	}
//	if ni.next != nil {
//		ni.next.prev = ni.prev
//	}
//	// if the removed item was at the head, we must update the head.
//	if ni == cache.headNode {
//		cache.headNode = ni.next
//	}
//	delete(cache.nodes, name)
//}

// Dump produces a dump of the current scheduler cache. This is used for
// debugging purposes only and shouldn't be confused with UpdateSnapshot
// function.
// This method is expensive, and should be only used in non-critical path.
func (cache *cacheImpl) Dump() *Dump {
	cache.mu.RLock()
	defer cache.mu.RUnlock()

	ctx := context.Background()

	// Ask the store for the currently “live” nodes.
	names, err := cache.nodeStore.ListLiveNames(ctx, 0)
	if err != nil {
		klog.FromContext(ctx).Error(err, "ListLiveNames failed in Dump")
		names = nil
	}

	nodes := make(map[string]*framework.NodeInfo, len(names))
	for _, name := range names {
		if ni, _, _, ok, err := cache.nodeStore.GetByName(ctx, name); err == nil && ok && ni != nil && ni.Node() != nil {
			nodes[name] = ni.Snapshot()
		}
	}

	_, assumed, _ := cache.podStateStore.List(ctx)

	return &Dump{
		Nodes:       nodes,
		AssumedPods: sets.KeySet(assumed),
	}
}

func (cache *cacheImpl) Snapshot() *Snapshot {
	return cache.snapshot
}

// UpdateSnapshot takes a snapshot of cached NodeInfo map. This is called at
// beginning of every scheduling cycle.
// The snapshot only includes Nodes that are not deleted at the time this function is called.
// nodeInfo.Node() is guaranteed to be not nil for all the nodes in the snapshot.
// This function tracks generation number of NodeInfo and updates only the
// entries of an existing snapshot that have changed after the snapshot was taken.
func (cache *cacheImpl) UpdateSnapshot(logger klog.Logger, nodeSnapshot *Snapshot) error {
	//cache.mu.Lock()
	//defer cache.mu.Unlock()
	//
	//// Get the last generation of the snapshot.
	//snapshotGeneration := nodeSnapshot.generation
	//
	//// NodeInfoList and HavePodsWithAffinityNodeInfoList must be re-created if a node was added
	//// or removed from the cache.
	//updateAllLists := false
	//// HavePodsWithAffinityNodeInfoList must be re-created if a node changed its
	//// status from having pods with affinity to NOT having pods with affinity or the other
	//// way around.
	//updateNodesHavePodsWithAffinity := false
	//// HavePodsWithRequiredAntiAffinityNodeInfoList must be re-created if a node changed its
	//// status from having pods with required anti-affinity to NOT having pods with required
	//// anti-affinity or the other way around.
	//updateNodesHavePodsWithRequiredAntiAffinity := false
	//// usedPVCSet must be re-created whenever the head node generation is greater than
	//// last snapshot generation.
	//updateUsedPVCSet := false
	//
	//const page int32 = 500
	//var maxSeenGen = snapshotGeneration
	//
	//for {
	//	names, maxGen, err := cache.nodeStore.ListChangedSinceGen(context.Background(), snapshotGeneration, page)
	//	if err != nil {
	//		return err
	//	}
	//	if len(names) == 0 {
	//		break
	//	}
	//	if maxGen > maxSeenGen {
	//		maxSeenGen = maxGen
	//	}
	//
	//	for _, name := range names {
	//		ni, gen, _, ok, err := cache.nodeStore.GetByName(context.Background(), name)
	//		if err != nil {
	//			logger.Error(err, "GetByName failed while updating snapshot", "node", klog.KRef("", name))
	//			continue
	//		}
	//		if !ok || ni == nil || ni.Node() == nil {
	//			// Ghost / deleted: ensure it drops out of the snapshot.
	//			if _, exists := nodeSnapshot.nodeInfoMap[name]; exists {
	//				delete(nodeSnapshot.nodeInfoMap, name)
	//				updateAllLists = true
	//			}
	//			continue
	//		}
	//
	//		existing, ok := nodeSnapshot.nodeInfoMap[name]
	//		if !ok {
	//			updateAllLists = true
	//			existing = &framework.NodeInfo{}
	//			nodeSnapshot.nodeInfoMap[name] = existing
	//		}
	//
	//		clone := ni.Snapshot()
	//
	//		if (len(existing.PodsWithAffinity) > 0) != (len(clone.PodsWithAffinity) > 0) {
	//			updateNodesHavePodsWithAffinity = true
	//		}
	//		if (len(existing.PodsWithRequiredAntiAffinity) > 0) != (len(clone.PodsWithRequiredAntiAffinity) > 0) {
	//			updateNodesHavePodsWithRequiredAntiAffinity = true
	//		}
	//		if !updateUsedPVCSet {
	//			if len(existing.PVCRefCounts) != len(clone.PVCRefCounts) {
	//				updateUsedPVCSet = true
	//			} else {
	//				for pvcKey := range clone.PVCRefCounts {
	//					if _, found := existing.PVCRefCounts[pvcKey]; !found {
	//						updateUsedPVCSet = true
	//						break
	//					}
	//				}
	//			}
	//		}
	//
	//		*existing = *clone
	//		if gen > nodeSnapshot.generation {
	//			nodeSnapshot.generation = gen
	//		}
	//	}
	//
	//	if int32(len(names)) < page {
	//		break
	//	}
	//}
	//
	//if maxSeenGen > nodeSnapshot.generation {
	//	nodeSnapshot.generation = maxSeenGen
	//}
	//
	//// Prune anything in the snapshot that’s no longer live.
	//liveNames, err := cache.nodeStore.ListLiveNames(context.Background(), 0)
	//if err == nil {
	//	live := sets.New[string](liveNames...)
	//	for name := range nodeSnapshot.nodeInfoMap {
	//		if !live.Has(name) {
	//			delete(nodeSnapshot.nodeInfoMap, name)
	//			updateAllLists = true
	//		}
	//	}
	//} else {
	//	logger.Error(err, "ListLiveNames failed while pruning snapshot")
	//}
	//
	//if updateAllLists || updateNodesHavePodsWithAffinity || updateNodesHavePodsWithRequiredAntiAffinity || updateUsedPVCSet {
	//	cache.updateNodeInfoSnapshotList(logger, nodeSnapshot, updateAllLists)
	//}

	return nil
}

//func (cache *cacheImpl) updateNodeInfoSnapshotList(logger klog.Logger, snapshot *Snapshot, updateAll bool) {
//	snapshot.havePodsWithAffinityNodeInfoList = make([]*framework.NodeInfo, 0, len(snapshot.nodeInfoMap))
//	snapshot.havePodsWithRequiredAntiAffinityNodeInfoList = make([]*framework.NodeInfo, 0, len(snapshot.nodeInfoMap))
//	snapshot.usedPVCSet = sets.New[string]()
//	if updateAll {
//		// Deterministic order: sort names.
//		names := make([]string, 0, len(snapshot.nodeInfoMap))
//		for n := range snapshot.nodeInfoMap {
//			names = append(names, n)
//		}
//		sort.Strings(names)
//
//		snapshot.nodeInfoList = make([]*framework.NodeInfo, 0, len(names))
//		for _, nodeName := range names {
//			nodeInfo := snapshot.nodeInfoMap[nodeName]
//			if nodeInfo == nil || nodeInfo.Node() == nil {
//				continue
//			}
//			snapshot.nodeInfoList = append(snapshot.nodeInfoList, nodeInfo)
//			if len(nodeInfo.PodsWithAffinity) > 0 {
//				snapshot.havePodsWithAffinityNodeInfoList = append(snapshot.havePodsWithAffinityNodeInfoList, nodeInfo)
//			}
//			if len(nodeInfo.PodsWithRequiredAntiAffinity) > 0 {
//				snapshot.havePodsWithRequiredAntiAffinityNodeInfoList = append(snapshot.havePodsWithRequiredAntiAffinityNodeInfoList, nodeInfo)
//			}
//			for key := range nodeInfo.PVCRefCounts {
//				snapshot.usedPVCSet.Insert(key)
//			}
//		}
//	} else {
//		for _, nodeInfo := range snapshot.nodeInfoList {
//			if nodeInfo == nil || nodeInfo.Node() == nil {
//				continue
//			}
//			if len(nodeInfo.PodsWithAffinity) > 0 {
//				snapshot.havePodsWithAffinityNodeInfoList = append(snapshot.havePodsWithAffinityNodeInfoList, nodeInfo)
//			}
//			if len(nodeInfo.PodsWithRequiredAntiAffinity) > 0 {
//				snapshot.havePodsWithRequiredAntiAffinityNodeInfoList = append(snapshot.havePodsWithRequiredAntiAffinityNodeInfoList, nodeInfo)
//			}
//			for key := range nodeInfo.PVCRefCounts {
//				snapshot.usedPVCSet.Insert(key)
//			}
//		}
//	}
//}
//
//// If certain nodes were deleted after the last snapshot was taken, we should remove them from the snapshot.
//func (cache *cacheImpl) removeDeletedNodesFromSnapshot(snapshot *Snapshot) {
//	// Build a set of *live* node names from the node store.
//	ctx := context.Background()
//	liveNames, err := cache.nodeStore.ListLiveNames(ctx, 0) // 0 = impl default paging
//	if err != nil {
//		// Conservative: if we can't list, don't delete anything.
//		klog.FromContext(ctx).Error(err, "ListLiveNames failed in removeDeletedNodesFromSnapshot")
//		return
//	}
//	live := sets.New[string](liveNames...)
//	for name := range snapshot.nodeInfoMap {
//		if !live.Has(name) {
//			delete(snapshot.nodeInfoMap, name)
//		}
//	}
//}

// NodeCount returns the number of nodes in the cache.
// DO NOT use outside of tests.
func (cache *cacheImpl) NodeCount() int {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	names, err := cache.nodeStore.ListLiveNames(context.Background(), 0)
	if err != nil {
		return 0
	}
	return len(names)
}

// PodCount returns the number of pods in the cache (including those from deleted nodes).
// DO NOT use outside of tests.
func (cache *cacheImpl) PodCount() (int, error) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	states, _, err := cache.podStateStore.List(context.Background())
	if err != nil {
		return 0, err
	}
	return len(states), nil
}

func (cache *cacheImpl) AssumePod(logger klog.Logger, pod *v1.Pod) error {
	key, err := framework.GetPodKey(pod)
	if err != nil {
		return err
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	//if _, ok := cache.podStates[key]; ok {
	//	return fmt.Errorf("pod %v(%v) is in the cache, so can't be assumed", key, klog.KObj(pod))
	//}
	if _, _, found, _ := cache.podStateStore.Get(context.Background(), key); found {
		return fmt.Errorf("pod %q already in cache", key)
	}

	return cache.addPod(logger, pod, true)
}

func (cache *cacheImpl) FinishBinding(logger klog.Logger, pod *v1.Pod) error {
	return cache.finishBinding(logger, pod, time.Now())
}

// finishBinding exists to make tests deterministic by injecting now as an argument
func (cache *cacheImpl) finishBinding(logger klog.Logger, pod *v1.Pod, now time.Time) error {
	key, err := framework.GetPodKey(pod)
	if err != nil {
		return err
	}

	cache.mu.RLock()
	defer cache.mu.RUnlock()

	logger.V(5).Info("Finished binding for pod, can be expired", "podKey", key, "pod", klog.KObj(pod))
	ps, assumed, found, err := cache.podStateStore.Get(context.Background(), key)
	if err != nil || !found {
		return fmt.Errorf("no podState for %q", key)
	}
	if assumed {
		if cache.ttl == 0 {
			ps.Deadline = nil
		} else {
			dl := now.Add(cache.ttl)
			ps.Deadline = &dl
		}
		ps.BindingFinished = true
		return cache.podStateStore.Put(context.Background(), key, ps, true)
	}
	return nil

	//currState, ok := cache.podStates[key]
	//if ok && cache.assumedPods.Has(key) {
	//	if cache.ttl == time.Duration(0) {
	//		currState.deadline = nil
	//	} else {
	//		dl := now.Add(cache.ttl)
	//		currState.deadline = &dl
	//	}
	//	currState.bindingFinished = true
	//}
	//return nil
}

func (cache *cacheImpl) ForgetPod(logger klog.Logger, pod *v1.Pod) error {
	key, err := framework.GetPodKey(pod)
	if err != nil {
		return err
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	ps, assumed, found, err := cache.podStateStore.Get(context.Background(), key)
	if err != nil {
		return err
	}
	if !found || !assumed {
		return fmt.Errorf("pod %q wasn't assumed", key)
	}
	if ps.Pod.Spec.NodeName != pod.Spec.NodeName {
		return fmt.Errorf("pod %q was assumed on %q but assigned to %q",
			key, ps.Pod.Spec.NodeName, pod.Spec.NodeName)
	}
	return cache.removePod(logger, pod)

	//currState, ok := cache.podStates[key]
	//if ok && currState.pod.Spec.NodeName != pod.Spec.NodeName {
	//	return fmt.Errorf("pod %v(%v) was assumed on %v but assigned to %v", key, klog.KObj(pod), pod.Spec.NodeName, currState.pod.Spec.NodeName)
	//}
	//
	//// Only assumed pod can be forgotten.
	//if ok && cache.assumedPods.Has(key) {
	//	return cache.removePod(logger, pod)
	//}
	//return fmt.Errorf("pod %v(%v) wasn't assumed so cannot be forgotten", key, klog.KObj(pod))
}

// Assumes that lock is already acquired.
func (cache *cacheImpl) addPod(logger klog.Logger, pod *v1.Pod, assumePod bool) error {
	key, err := framework.GetPodKey(pod)
	if err != nil {
		return err
	}

	ctx := context.Background()

	ni, _, _, ok, err := cache.nodeStore.GetByName(ctx, pod.Spec.NodeName)
	if err != nil || !ok {
		if err != nil {
			logger.Error(err, "nodeStore.GetByName failed in addPod", "node", klog.KRef("", pod.Spec.NodeName))
		}
		ni = framework.NewNodeInfo()
	}

	ni.AddPod(pod) // bumps Generation
	if err := cache.nodeStore.Put(ctx, pod.Spec.NodeName, ni, ni.Generation, time.Now()); err != nil {
		logger.Error(err, "nodeStore.Put failed in addPod", "node", klog.KRef("", pod.Spec.NodeName))
		// continue: we still track pod state so cleanup works
	}

	ps := &awsstore.PodState{Pod: pod}
	return cache.podStateStore.Put(ctx, key, ps, assumePod)
}

// Assumes that lock is already acquired.
func (cache *cacheImpl) updatePod(logger klog.Logger, oldPod, newPod *v1.Pod) error {
	if err := cache.removePod(logger, oldPod); err != nil {
		return err
	}
	return cache.addPod(logger, newPod, false)
}

// Assumes that lock is already acquired.
// Removes a pod from the cached node info. If the node information was already
// removed and there are no more pods left in the node, cleans up the node from
// the cache.
func (cache *cacheImpl) removePod(logger klog.Logger, pod *v1.Pod) error {
	key, err := framework.GetPodKey(pod)
	if err != nil {
		return err
	}

	//n, ok := cache.nodes[pod.Spec.NodeName]
	//if !ok {
	//	logger.Error(nil, "Node not found when trying to remove pod", "node", klog.KRef("", pod.Spec.NodeName), "podKey", key, "pod", klog.KObj(pod))
	//} else {
	//	if err := n.info.RemovePod(logger, pod); err != nil {
	//		return err
	//	}
	//	if len(n.info.Pods) == 0 && n.info.Node() == nil {
	//		cache.removeNodeInfoFromList(logger, pod.Spec.NodeName)
	//	} else {
	//		cache.moveNodeInfoToHead(logger, pod.Spec.NodeName)
	//	}
	//}
	//
	//_ = cache.podStateStore.Delete(context.Background(), key)
	//return nil

	// 1) Load NodeInfo from NodeStore.
	ni, _, _, ok, err := cache.nodeStore.GetByName(context.TODO(), pod.Spec.NodeName)
	if err != nil {
		logger.Error(err, "nodeStore.GetByName failed in removePod", "node", klog.KRef("", pod.Spec.NodeName))
		// still remove podstate; nothing else we can do
		_ = cache.podStateStore.Delete(context.TODO(), key)
		return nil
	}
	if ok {
		// 2) Remove pod and persist.
		if err := ni.RemovePod(logger, pod); err != nil {
			return err
		}
		if err := cache.nodeStore.Put(context.TODO(), pod.Spec.NodeName, ni, ni.Generation, time.Now()); err != nil {
			logger.Error(err, "nodeStore.Put failed in removePod", "node", klog.KRef("", pod.Spec.NodeName))
		}
	}

	// 3) Delete pod state entry.
	return cache.podStateStore.Delete(context.TODO(), key)
}

func (cache *cacheImpl) AddPod(logger klog.Logger, pod *v1.Pod) error {
	key, err := framework.GetPodKey(pod)
	if err != nil {
		return err
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	ps, assumed, found, _ := cache.podStateStore.Get(context.TODO(), key)

	switch {
	case found && assumed:
		// Already assumed; just update.
		if err := cache.updatePod(logger, ps.Pod, pod); err != nil {
			logger.Error(err, "Error occurred while updating pod")
		}
		if ps.Pod.Spec.NodeName != pod.Spec.NodeName {
			// Swap labels: ps is the assumed state; pod is the current event.
			logger.Info("Pod was added to a different node than it was assumed",
				"podKey", key, "pod", klog.KObj(pod),
				"assumedNode", klog.KRef("", ps.Pod.Spec.NodeName),
				"currentNode", klog.KRef("", pod.Spec.NodeName))
		}
	case !found:
		// Expired or never added.
		if err := cache.addPod(logger, pod, false); err != nil {
			logger.Error(err, "Error occurred while adding pod")
		}
	//currState, ok := cache.podStates[key]
	//switch {
	//case ok && cache.assumedPods.Has(key):
	//	// When assuming, we've already added the Pod to cache,
	//	// Just update here to make sure the Pod's status is up-to-date.
	//	if err = cache.updatePod(logger, currState.pod, pod); err != nil {
	//		logger.Error(err, "Error occurred while updating pod")
	//	}
	//	if currState.pod.Spec.NodeName != pod.Spec.NodeName {
	//		// The pod was added to a different node than it was assumed to.
	//		logger.Info("Pod was added to a different node than it was assumed", "podKey", key, "pod", klog.KObj(pod), "assumedNode", klog.KRef("", pod.Spec.NodeName), "currentNode", klog.KRef("", currState.pod.Spec.NodeName))
	//		return nil
	//	}
	//case !ok:
	//	// Pod was expired. We should add it back.
	//	if err = cache.addPod(logger, pod, false); err != nil {
	//		logger.Error(err, "Error occurred while adding pod")
	//	}
	default:
		return fmt.Errorf("pod %v(%v) was already in added state", key, klog.KObj(pod))
	}
	return nil
}

func (cache *cacheImpl) UpdatePod(logger klog.Logger, oldPod, newPod *v1.Pod) error {
	key, err := framework.GetPodKey(oldPod)
	if err != nil {
		return err
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	ps, assumed, found, _ := cache.podStateStore.Get(context.Background(), key)
	if !found {
		return fmt.Errorf("pod %v(%v) is not added to scheduler cache, so cannot be updated",
			key, klog.KObj(oldPod))
	}
	if assumed {
		return fmt.Errorf("assumed pod %v(%v) should not be updated", key, klog.KObj(oldPod))
	}
	//currState, ok := cache.podStates[key]
	//if !ok {
	//	return fmt.Errorf("pod %v(%v) is not added to scheduler cache, so cannot be updated", key, klog.KObj(oldPod))
	//}
	//
	//// An assumed pod won't have Update/Remove event. It needs to have Add event
	//// before Update event, in which case the state would change from Assumed to Added.
	//if cache.assumedPods.Has(key) {
	//	return fmt.Errorf("assumed pod %v(%v) should not be updated", key, klog.KObj(oldPod))
	//}

	if ps.Pod.Spec.NodeName != newPod.Spec.NodeName {
		logger.Error(nil, "Pod updated on a different node than previously added to", "podKey", key, "pod", klog.KObj(oldPod))
		logger.Error(nil, "scheduler cache is corrupted and can badly affect scheduling decisions")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}
	return cache.updatePod(logger, oldPod, newPod)
}

func (cache *cacheImpl) RemovePod(logger klog.Logger, pod *v1.Pod) error {
	key, err := framework.GetPodKey(pod)
	if err != nil {
		return err
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	ps, _, found, _ := cache.podStateStore.Get(context.Background(), key)
	if !found {
		return fmt.Errorf("pod %v(%v) is not found in scheduler cache, so cannot be removed",
			key, klog.KObj(pod))
	}
	if ps.Pod.Spec.NodeName != pod.Spec.NodeName {
		logger.Error(nil, "Pod was added to a different node than it is being removed from",
			"podKey", key, "pod", klog.KObj(pod),
			// ps is the cached/current location; the event carries the (possibly empty) node
			"currentNode", klog.KRef("", ps.Pod.Spec.NodeName),
			"eventNode", klog.KRef("", pod.Spec.NodeName))
		if pod.Spec.NodeName != "" {
			logger.Error(nil, "scheduler cache is corrupted and can badly affect scheduling decisions")
			klog.FlushAndExit(klog.ExitFlushTimeout, 1)
		}
	}
	return cache.removePod(logger, ps.Pod)
}

func (cache *cacheImpl) IsAssumedPod(pod *v1.Pod) (bool, error) {
	key, err := framework.GetPodKey(pod)
	if err != nil {
		return false, err
	}

	cache.mu.RLock()
	defer cache.mu.RUnlock()

	_, assumed, found, err := cache.podStateStore.Get(context.Background(), key)
	if err != nil || !found {
		return false, err
	}
	return assumed, nil
	//return cache.assumedPods.Has(key), nil
}

// GetPod might return a pod for which its node has already been deleted from
// the main cache. This is useful to properly process pod update events.
func (cache *cacheImpl) GetPod(pod *v1.Pod) (*v1.Pod, error) {
	key, err := framework.GetPodKey(pod)
	if err != nil {
		return nil, err
	}

	cache.mu.RLock()
	defer cache.mu.RUnlock()

	//podState, ok := cache.podStates[key]
	//if !ok {
	//	return nil, fmt.Errorf("pod %v(%v) does not exist in scheduler cache", key, klog.KObj(pod))
	//}
	//
	//return podState.pod, nil
	ps, _, found, _ := cache.podStateStore.Get(context.Background(), key)
	if !found {
		return nil, fmt.Errorf("pod %v(%v) does not exist in scheduler cache", key, klog.KObj(pod))
	}
	return ps.Pod, nil
}

func (cache *cacheImpl) AddNode(logger klog.Logger, node *v1.Node) *framework.NodeInfo {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	//n, ok := cache.nodes[node.Name]
	//if !ok {
	//	n = newNodeInfoListItem(framework.NewNodeInfo())
	//	cache.nodes[node.Name] = n
	//} else {
	//	//cache.removeNodeImageStates(n.info.Node())
	//}
	//cache.moveNodeInfoToHead(logger, node.Name)
	// Get current NodeInfo from the store (or create a new one).
	ni, _, _, ok, err := cache.nodeStore.GetByName(context.TODO(), node.Name)
	if err != nil {
		logger.Error(err, "nodeStore.GetByName failed in AddNode", "node", klog.KRef("", node.Name))
		ni = framework.NewNodeInfo()
	}
	if !ok {
		ni = framework.NewNodeInfo()
	}

	//n.info.SetNode(node)
	//return n.info.Snapshot()
	// Write node into NodeInfo and persist.
	ni.SetNode(node) // bumps Generation
	if err := cache.nodeStore.Put(context.TODO(), node.Name, ni, ni.Generation, time.Now()); err != nil {
		logger.Error(err, "nodeStore.Put failed in AddNode", "node", klog.KRef("", node.Name))
	}

	return ni.Snapshot()
}

func (cache *cacheImpl) UpdateNode(logger klog.Logger, oldNode, newNode *v1.Node) *framework.NodeInfo {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	//n, ok := cache.nodes[newNode.Name]
	//if !ok {
	//	n = newNodeInfoListItem(framework.NewNodeInfo())
	//	cache.nodes[newNode.Name] = n
	//	cache.nodeTree.addNode(logger, newNode)
	//} else {
	//	//cache.removeNodeImageStates(n.info.Node())
	//}
	//cache.moveNodeInfoToHead(logger, newNode.Name)
	// Get current NodeInfo from the store (or create a new one if missing).
	ni, _, _, ok, err := cache.nodeStore.GetByName(context.TODO(), newNode.Name)
	if err != nil {
		logger.Error(err, "nodeStore.GetByName failed in UpdateNode", "node", klog.KRef("", newNode.Name))
		ni = framework.NewNodeInfo()
	}
	if !ok {
		ni = framework.NewNodeInfo()
	}

	//n.info.SetNode(newNode)
	//return n.info.Snapshot()
	// Apply new object; persist.
	ni.SetNode(newNode) // bumps Generation
	if err := cache.nodeStore.Put(context.TODO(), newNode.Name, ni, ni.Generation, time.Now()); err != nil {
		logger.Error(err, "nodeStore.Put failed in UpdateNode", "node", klog.KRef("", newNode.Name))
	}

	return ni.Snapshot()
}

// RemoveNode removes a node from the cache's tree.
// The node might still have pods because their deletion events didn't arrive
// yet. Those pods are considered removed from the cache, being the node tree
// the source of truth.
// However, we keep a ghost node with the list of pods until all pod deletion
// events have arrived. A ghost node is skipped from snapshots.
func (cache *cacheImpl) RemoveNode(logger klog.Logger, node *v1.Node) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	//n, ok := cache.nodes[node.Name]
	//if !ok {
	//	return fmt.Errorf("node %v is not found", node.Name)
	//}
	//n.info.RemoveNode()
	//// We remove NodeInfo for this node only if there aren't any pods on this node.
	//// We can't do it unconditionally, because notifications about pods are delivered
	//// in a different watch, and thus can potentially be observed later, even though
	//// they happened before node removal.
	//if len(n.info.Pods) == 0 {
	//	cache.removeNodeInfoFromList(logger, node.Name)
	//} else {
	//	cache.moveNodeInfoToHead(logger, node.Name)
	//}
	//if err := cache.nodeTree.removeNode(logger, node); err != nil {
	//	return err
	//}
	////cache.removeNodeImageStates(node)
	//return nil

	// Load NodeInfo from the store.
	ni, _, _, ok, err := cache.nodeStore.GetByName(context.TODO(), node.Name)
	if err != nil {
		logger.Error(err, "nodeStore.GetByName failed in RemoveNode", "node", klog.KRef("", node.Name))
		return fmt.Errorf("node %v is not found", node.Name)
	}
	if !ok {
		return fmt.Errorf("node %v is not found", node.Name)
	}

	// Turn this NodeInfo into a ghost (keep pods list if any, clear Node()).
	ni.RemoveNode() // does not clear pods

	// Persist the ghost (or empty) record. We keep it even if no pods remain,
	// which is harmless. If you prefer to delete rows when empty, add a Delete(name) to NodeStore.
	if err := cache.nodeStore.Put(context.TODO(), node.Name, ni, ni.Generation, time.Now()); err != nil {
		logger.Error(err, "nodeStore.Put failed in RemoveNode", "node", klog.KRef("", node.Name))
	}

	return nil
}

func (cache *cacheImpl) run(logger klog.Logger) {
	go wait.Until(func() {
		cache.cleanupAssumedPods(logger, time.Now())
	}, cache.period, cache.stop)
}

// cleanupAssumedPods exists for making test deterministic by taking time as input argument.
// It also reports metrics on the cache size for nodes, pods, and assumed pods.
func (cache *cacheImpl) cleanupAssumedPods(logger klog.Logger, now time.Time) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	//defer cache.updateMetrics()

	//// The size of assumedPods should be small
	//for key := range cache.assumedPods {
	//	ps, ok := cache.podStates[key]
	//	if !ok {
	//		logger.Error(nil, "Key found in assumed set but not in podStates, potentially a logical error")
	//		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	//	}
	//	if !ps.bindingFinished {
	//		logger.V(5).Info("Could not expire cache for pod as binding is still in progress", "podKey", key, "pod", klog.KObj(ps.pod))
	//		continue
	//	}
	//	if cache.ttl != 0 && now.After(*ps.deadline) {
	//		logger.Info("Pod expired", "podKey", key, "pod", klog.KObj(ps.pod))
	//		if err := cache.removePod(logger, ps.pod); err != nil {
	//			logger.Error(err, "ExpirePod failed", "podKey", key, "pod", klog.KObj(ps.pod))
	//		}
	//	}
	//}

	states, assumedSet, err := cache.podStateStore.List(context.Background())
	if err != nil {
		logger.Error(err, "Failed to list pod states from store")
		return
	}

	for key := range assumedSet {
		ps, ok := states[key]
		if !ok {
			logger.Error(nil, "Key found in assumed set but not in podStates, logical error", "podKey", key)
			klog.FlushAndExit(klog.ExitFlushTimeout, 1)
		}

		if !ps.BindingFinished {
			logger.V(5).Info("Could not expire cache for pod as binding is still in progress",
				"podKey", key, "pod", klog.KObj(ps.Pod))
			continue
		}

		if cache.ttl != 0 && ps.Deadline != nil && now.After(*ps.Deadline) {
			logger.Info("Pod expired", "podKey", key, "pod", klog.KObj(ps.Pod))
			if err := cache.removePod(logger, ps.Pod); err != nil {
				logger.Error(err, "ExpirePod failed", "podKey", key, "pod", klog.KObj(ps.Pod))
			}
		}
	}
}
