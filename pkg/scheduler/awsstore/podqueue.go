package awsstore

import (
	"context"

	"k8s.io/kubernetes/pkg/scheduler/backend/heap"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

/*
PodQueue is the tiny abstraction layer the scheduler uses instead of the
concrete *heap.Heap.  Anything implementing this interface can serve as the
“active Q”.
*/
type PodQueue interface {
	AddOrUpdate(*framework.QueuedPodInfo)
	Get(*framework.QueuedPodInfo) (*framework.QueuedPodInfo, bool)
	Has(*framework.QueuedPodInfo) bool
	Delete(*framework.QueuedPodInfo) error
	Pop() (*framework.QueuedPodInfo, error)
	Peek() (*framework.QueuedPodInfo, bool)
	List() []*framework.QueuedPodInfo
	Len() int
}

//
// -------------------------  in-memory adapter  ----------------------------- //
//

type HeapPQ struct {
	H *heap.Heap[*framework.QueuedPodInfo]
} // ← exported

func (q HeapPQ) AddOrUpdate(p *framework.QueuedPodInfo)                          { q.H.AddOrUpdate(p) }
func (q HeapPQ) Get(p *framework.QueuedPodInfo) (*framework.QueuedPodInfo, bool) { return q.H.Get(p) }
func (q HeapPQ) Has(p *framework.QueuedPodInfo) bool                             { return q.H.Has(p) }
func (q HeapPQ) Delete(p *framework.QueuedPodInfo) error                         { return q.H.Delete(p) }
func (q HeapPQ) Pop() (*framework.QueuedPodInfo, error)                          { return q.H.Pop() }
func (q HeapPQ) Peek() (*framework.QueuedPodInfo, bool)                          { return q.H.Peek() }
func (q HeapPQ) List() []*framework.QueuedPodInfo                                { return q.H.List() }
func (q HeapPQ) Len() int                                                        { return q.H.Len() }

//
// ---------------------------  DynamoDB adapter  --------------------------- //
//

type DDBPQ struct {
	Ctx context.Context
	Aws *PriorityQueueAWS
}

func (q DDBPQ) AddOrUpdate(p *framework.QueuedPodInfo) { _ = q.Aws.AddOrUpdate(q.Ctx, p) }
func (q DDBPQ) Get(*framework.QueuedPodInfo) (*framework.QueuedPodInfo, bool) {
	return nil, false // no point-lookup support
}
func (q DDBPQ) Has(*framework.QueuedPodInfo) bool       { return false }
func (q DDBPQ) Delete(p *framework.QueuedPodInfo) error { return q.Aws.Delete(q.Ctx, p) }
func (q DDBPQ) Pop() (*framework.QueuedPodInfo, error)  { return q.Aws.Pop(q.Ctx) }
func (q DDBPQ) Peek() (*framework.QueuedPodInfo, bool) {
	pInfo, err := q.Aws.Peek(q.Ctx)
	if err != nil || pInfo == nil {
		return nil, false
	}
	return pInfo, true
}
func (q DDBPQ) List() []*framework.QueuedPodInfo { out, _ := q.Aws.List(q.Ctx); return out }
func (q DDBPQ) Len() int                         { n, _ := q.Aws.Count(q.Ctx); return n }
