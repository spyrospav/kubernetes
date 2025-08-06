package awsstore

import (
	"context"
	"encoding/json"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"k8s.io/apimachinery/pkg/types"
	"path"
	"slices"
)

const (
	pkByNode = "nominatedByNode" // SK = "<node>/<uid>" → payload = podRef JSON
	pkByUID  = "nominatedByUID"  // SK = "<uid>"       → payload = nodeName bytes
)

type NominatedStore interface {
	Put(ctx context.Context, uid types.UID, nodeName string, pr PodRef) error
	Delete(ctx context.Context, uid types.UID, nodeName string) error
	ListByNode(ctx context.Context, nodeName string) ([]PodRef, error)
	GetNodeForUID(ctx context.Context, uid types.UID) (string, bool, error)
	Clear(ctx context.Context) error
}

//
// -------------------------  in-memory adapter  ----------------------------- //
//

type MemNominatedStore struct {
	byNode map[string][]PodRef // node -> []podRef
	byUID  map[types.UID]string
}

func NewMemNominatedStore() *MemNominatedStore {
	return &MemNominatedStore{
		byNode: make(map[string][]PodRef),
		byUID:  make(map[types.UID]string),
	}
}

func (m *MemNominatedStore) Put(_ context.Context, uid types.UID, node string, pr PodRef) error {
	// remove old
	if old, ok := m.byUID[uid]; ok {
		list := m.byNode[old]
		for i := range list {
			if list[i].uid == uid {
				m.byNode[old] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(m.byNode[old]) == 0 {
			delete(m.byNode, old)
		}
	}
	m.byUID[uid] = node
	m.byNode[node] = append(m.byNode[node], pr)
	return nil
}

func (m *MemNominatedStore) Delete(_ context.Context, uid types.UID, node string) error {
	if node == "" {
		node = m.byUID[uid]
	}
	if node != "" {
		list := m.byNode[node]
		for i := range list {
			if list[i].uid == uid {
				m.byNode[node] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(m.byNode[node]) == 0 {
			delete(m.byNode, node)
		}
	}
	delete(m.byUID, uid)
	return nil
}

func (m *MemNominatedStore) ListByNode(_ context.Context, node string) ([]PodRef, error) {
	return slices.Clone(m.byNode[node]), nil
}

func (m *MemNominatedStore) GetNodeForUID(_ context.Context, uid types.UID) (string, bool, error) {
	n, ok := m.byUID[uid]
	return n, ok, nil
}

func (m *MemNominatedStore) Clear(_ context.Context) error {
	m.byNode = make(map[string][]PodRef)
	m.byUID = make(map[types.UID]string)
	return nil
}

//
// ---------------------------  DynamoDB adapter  --------------------------- //
//

type DDBNominatedStore struct {
	ctx      context.Context
	byNodePS *PartitionStore
	byUIDPS  *PartitionStore
}

func NewDDBNominatedStore(ctx context.Context, ddb *dynamodb.Client, wipe bool) (*DDBNominatedStore, error) {
	// same shared table; different PKs
	byNode, err := NewPartitionStore(ctx, ddb, PartitionStoreTableName, pkByNode, 0, wipe)
	if err != nil {
		return nil, err
	}
	byUID, err := NewPartitionStore(ctx, ddb, PartitionStoreTableName, pkByUID, 0, wipe)
	if err != nil {
		return nil, err
	}
	return &DDBNominatedStore{ctx: ctx, byNodePS: byNode, byUIDPS: byUID}, nil
}

func (s *DDBNominatedStore) Put(ctx context.Context, uid types.UID, node string, pr PodRef) error {
	// forward index: by node
	key := path.Join(node, string(uid)) // "<node>/<uid>"
	b, _ := json.Marshal(wireNom{Pod: pr})
	if err := s.byNodePS.Put(ctx, key, b); err != nil {
		return err
	}
	// reverse index: by uid
	return s.byUIDPS.Put(ctx, string(uid), []byte(node))
}

func (s *DDBNominatedStore) Delete(ctx context.Context, uid types.UID, node string) error {
	// If node is unknown, try reverse lookup:
	if node == "" {
		if n, ok, _ := s.byUIDPS.Get(ctx, string(uid)); ok {
			node = string(n)
		}
	}
	if node != "" {
		_ = s.byNodePS.Delete(ctx, path.Join(node, string(uid)))
	}
	return s.byUIDPS.Delete(ctx, string(uid))
}

func (s *DDBNominatedStore) ListByNode(ctx context.Context, node string) ([]PodRef, error) {
	raw, err := s.byNodePS.ListByPrefix(ctx, node+"/")
	if err != nil {
		return nil, err
	}
	out := make([]PodRef, 0, len(raw))
	for _, v := range raw {
		var w wireNom
		if err := json.Unmarshal(v, &w); err == nil {
			out = append(out, w.Pod)
		}
	}
	return out, nil
}

func (s *DDBNominatedStore) GetNodeForUID(ctx context.Context, uid types.UID) (string, bool, error) {
	b, ok, err := s.byUIDPS.Get(ctx, string(uid))
	if err != nil || !ok {
		return "", false, err
	}
	return string(b), true, nil
}

func (s *DDBNominatedStore) Clear(ctx context.Context) error {
	if err := s.byNodePS.Clear(ctx); err != nil {
		return err
	}
	return s.byUIDPS.Clear(ctx)
}
