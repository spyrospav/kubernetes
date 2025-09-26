package awsstore

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type PutMode int

const (
	PutCreate PutMode = iota // precondition: not exists (version == 0)
	PutUpdate                // precondition: exists + CAS on version
	PutUpsert                // optional: exists? CAS update : create
)

type PutOptions struct {
	Mode        PutMode // Create or Update (recommend NOT using Upsert for hot paths)
	KeepAssumed bool    // whether the item remains in the "assumed" set
}

type PodStateStore interface {
	Put(ctx context.Context, key string, ps *PodState, opts PutOptions) error
	Get(ctx context.Context, key string) (*PodState, int64 /*version*/, bool /*assumed*/, bool /*found*/, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context) (map[string]*PodState, map[string]struct{} /*assumedSet*/, error)
	Clear(ctx context.Context) error
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

func (s *DDBPodStateStore) Put(ctx context.Context, key string, st *PodState, opts PutOptions) error {
	b, err := MarshalPodState(st, opts.KeepAssumed)
	if err != nil {
		return err
	}

	switch opts.Mode {
	case PutCreate:
		if err := s.ps.PutCreate(ctx, key, b, opts.KeepAssumed); err != nil {
			return err
		}
		st.Version = 1
		return nil

	case PutUpdate:
		if err := s.ps.PutUpdateCAS(ctx, key, b, opts.KeepAssumed, st.Version); err != nil {
			return err
		}
		st.Version++
		return nil

	case PutUpsert:
		if err := s.ps.PutUpsertCAS(ctx, key, b, opts.KeepAssumed, st.Version); err != nil {
			return err
		}
		// Unknown if we created (->1) or updated (->exp+1), but both end up +1 from current view:
		st.Version++
		return nil
	default:
		return fmt.Errorf("unknown PutMode %v", opts.Mode)
	}
}

func (s *DDBPodStateStore) Get(ctx context.Context, key string) (*PodState, int64, bool, bool, error) {
	b, ver, found, err := s.ps.Get(ctx, key)
	if err != nil || !found {
		return nil, 0, false, found, err
	}
	st, assumed, err := UnmarshalPodState(b)
	if err != nil {
		return nil, 0, false, true, err
	}
	st.Version = ver
	return st, ver, assumed, true, nil
}

func (s *DDBPodStateStore) Delete(ctx context.Context, key string) error {
	// Read current version first (no need to unmarshal just to get Ver)
	_, ver, found, err := s.ps.Get(ctx, key)
	if err != nil {
		return err
	}
	// Treat "not found" as success (idempotent delete)
	if !found {
		return nil
	}

	// Try a few times in case someone updates between our Get and Delete
	backoff := []time.Duration{20 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond}
	for i := 0; ; i++ {
		err = s.ps.DeleteIfVersion(ctx, key, ver)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrConflict) && i < len(backoff) {
			// Re-fetch latest version and retry
			_, _, found, gerr := s.ps.Get(ctx, key)
			if gerr != nil {
				return gerr
			}
			// If item disappeared while retrying, consider delete done
			if !found {
				return nil
			}
			time.Sleep(backoff[i])
			continue
		}
		return err
	}
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
