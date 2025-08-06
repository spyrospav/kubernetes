package awsstore

import (
	"context"
)

type PodStateStore interface {
	Put(ctx context.Context, key string, ps *PodState, assumed bool) error
	Get(ctx context.Context, key string) (*PodState, bool /*assumed*/, bool /*found*/, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context) (map[string]*PodState, map[string]struct{} /*assumedSet*/, error)
	Clear(ctx context.Context) error
}

//
// -------------------------  in-memory adapter  ----------------------------- //
//

type InMemoryPodStateStore struct {
	states  map[string]*PodState
	assumed map[string]struct{}
}

func NewInMemoryPodStateStore() *InMemoryPodStateStore {
	return &InMemoryPodStateStore{
		states:  make(map[string]*PodState),
		assumed: make(map[string]struct{}),
	}
}

// Put inserts or updates a podState and its assumed flag.
func (m *InMemoryPodStateStore) Put(_ context.Context, key string, ps *PodState, assumed bool) error {
	m.states[key] = ps
	if assumed {
		m.assumed[key] = struct{}{}
	} else {
		delete(m.assumed, key)
	}
	return nil
}

// Get returns (state, assumed?, found?, err)
func (m *InMemoryPodStateStore) Get(_ context.Context, key string) (*PodState, bool, bool, error) {
	ps, ok := m.states[key]
	if !ok {
		return nil, false, false, nil
	}
	_, as := m.assumed[key]
	return ps, as, true, nil
}

func (m *InMemoryPodStateStore) Delete(_ context.Context, key string) error {
	delete(m.states, key)
	delete(m.assumed, key)
	return nil
}

func (m *InMemoryPodStateStore) List(_ context.Context) (map[string]*PodState, map[string]struct{}, error) {
	// Return shallow copies so callers can mutate safely.
	st := make(map[string]*PodState, len(m.states))
	for k, v := range m.states {
		st[k] = v
	}
	as := make(map[string]struct{}, len(m.assumed))
	for k := range m.assumed {
		as[k] = struct{}{}
	}
	return st, as, nil
}

func (m *InMemoryPodStateStore) Clear(_ context.Context) error {
	m.states = make(map[string]*PodState)
	m.assumed = make(map[string]struct{})
	return nil
}

//
// ---------------------------  DynamoDB adapter  --------------------------- //
//

type DDBPodStateStore struct {
	ctx context.Context
	ps  *PartitionStore
}

func NewDDBPodStateStore(ctx context.Context, ps *PartitionStore) *DDBPodStateStore {
	return &DDBPodStateStore{
		ctx: ctx,
		ps:  ps,
	}
}

func (s *DDBPodStateStore) Put(ctx context.Context, key string, st *PodState, assumed bool) error {
	b, err := MarshalPodState(st, assumed)
	if err != nil {
		return err
	}
	return s.ps.Put(ctx, key, b)
}

func (s *DDBPodStateStore) Get(ctx context.Context, key string) (*PodState, bool, bool, error) {
	b, found, err := s.ps.Get(ctx, key)
	if err != nil || !found {
		return nil, false, found, err
	}
	st, assumed, err := UnmarshalPodState(b)
	return st, assumed, true, err
}

func (s *DDBPodStateStore) Delete(ctx context.Context, key string) error {
	return s.ps.Delete(ctx, key)
}

func (s *DDBPodStateStore) List(ctx context.Context) (map[string]*PodState, map[string]struct{}, error) {
	raw, err := s.ps.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	states := make(map[string]*PodState, len(raw))
	assumed := make(map[string]struct{}, len(raw))
	for k, v := range raw {
		st, as, err := UnmarshalPodState(v)
		if err != nil {
			return nil, nil, err
		}
		states[k] = st
		if as {
			assumed[k] = struct{}{}
		}
	}
	return states, assumed, nil
}

func (s *DDBPodStateStore) Clear(ctx context.Context) error {
	return s.ps.Clear(ctx)
}
