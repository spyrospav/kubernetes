package awsstore

import (
	"context"

	"k8s.io/kubernetes/pkg/scheduler/framework"
)

type PodStore interface {
	AddOrUpdate(key string, pInfo *framework.QueuedPodInfo) error
	Delete(key string) error
	Get(key string) (*framework.QueuedPodInfo, bool, error)
	Clear() error
	List() ([]*framework.QueuedPodInfo, error)
	ListWithKeys() (map[string]*framework.QueuedPodInfo, error)
}

//
// -------------------------  in-memory adapter  ----------------------------- //
//

type MapStore struct {
	store map[string]*framework.QueuedPodInfo
}

func NewMapStore() *MapStore {
	return &MapStore{
		store: make(map[string]*framework.QueuedPodInfo),
	}
}

func (s *MapStore) AddOrUpdate(key string, pInfo *framework.QueuedPodInfo) error {
	s.store[key] = pInfo
	return nil
}

func (s *MapStore) Delete(key string) error {
	delete(s.store, key)
	return nil
}

func (s *MapStore) Get(key string) (*framework.QueuedPodInfo, bool, error) {
	pInfo, exists := s.store[key]
	if !exists {
		return nil, false, nil
	}
	return pInfo, true, nil
}

func (s *MapStore) Clear() error {
	s.store = make(map[string]*framework.QueuedPodInfo)
	return nil
}

func (s *MapStore) List() ([]*framework.QueuedPodInfo, error) {
	out := make([]*framework.QueuedPodInfo, 0, len(s.store))
	for _, v := range s.store {
		out = append(out, v)
	}
	return out, nil
}

func (s *MapStore) ListWithKeys() (map[string]*framework.QueuedPodInfo, error) {
	out := make(map[string]*framework.QueuedPodInfo, len(s.store))
	for k, v := range s.store {
		out[k] = v
	}
	return out, nil
}

//
// ---------------------------  DynamoDB adapter  --------------------------- //
//

// DDBPodStore implements PodStore on top of PartitionStore.
type DDBPodStore struct {
	ctx context.Context
	ps  *PartitionStore
}

func NewDDBPodStore(ctx context.Context, ps *PartitionStore) *DDBPodStore {
	return &DDBPodStore{ctx: ctx, ps: ps}
}

func (s *DDBPodStore) AddOrUpdate(key string, p *framework.QueuedPodInfo) error {
	b, err := MarshalQueuedPodInfo(p)
	if err != nil {
		return err
	}
	return s.ps.Put(s.ctx, key, b)
}

func (s *DDBPodStore) Delete(key string) error {
	return s.ps.Delete(s.ctx, key)
}

func (s *DDBPodStore) Get(key string) (*framework.QueuedPodInfo, bool, error) {
	b, _, ok, err := s.ps.Get(s.ctx, key)
	if err != nil || !ok {
		return nil, ok, err
	}
	q, err := UnmarshalQueuedPodInfo(b)
	if err != nil {
		return nil, false, err
	}
	return q, true, nil
}

func (s *DDBPodStore) Clear() error {
	return s.ps.Clear(s.ctx)
}

func (s *DDBPodStore) List() ([]*framework.QueuedPodInfo, error) {
	m, err := s.ps.List(s.ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*framework.QueuedPodInfo, 0, len(m))
	for _, b := range m {
		q, err := UnmarshalQueuedPodInfo(b)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, nil
}

func (s *DDBPodStore) ListWithKeys() (map[string]*framework.QueuedPodInfo, error) {
	raw, err := s.ps.List(s.ctx) // returns map[string][]byte
	if err != nil {
		return nil, err
	}
	out := make(map[string]*framework.QueuedPodInfo, len(raw))
	for k, b := range raw {
		q, err := UnmarshalQueuedPodInfo(b)
		if err != nil {
			return nil, err
		}
		out[k] = q
	}
	return out, nil
}
