package awsstore

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sort"
	"time"
)

type NodeStore interface {
	Put(ctx context.Context, name string, ni *framework.NodeInfo, gen int64, ts time.Time) error
	GetByName(ctx context.Context, name string) (*framework.NodeInfo, int64, time.Time, bool, error)
	ListChangedSinceGen(ctx context.Context, minGen int64, pageSize int32) (names []string, maxGen int64, err error)
	Clear(ctx context.Context) error
	ListLiveNames(ctx context.Context, pageSize int32) ([]string, error)
}

//
// -------------------------  in-memory adapter  ----------------------------- //
//

type memNode struct {
	ni  *framework.NodeInfo
	gen int64
	ts  time.Time
}

type MemNodeStore struct {
	nodes map[string]*memNode
}

func NewMemNodeStore() *MemNodeStore {
	return &MemNodeStore{nodes: make(map[string]*memNode)}
}

func (m *MemNodeStore) Put(_ context.Context, name string, ni *framework.NodeInfo, gen int64, ts time.Time) error {
	m.nodes[name] = &memNode{
		ni:  ni.Snapshot(), // store an independent copy
		gen: gen,
		ts:  ts,
	}
	return nil
}

func (m *MemNodeStore) GetByName(_ context.Context, name string) (*framework.NodeInfo, int64, time.Time, bool, error) {
	n, ok := m.nodes[name]
	if !ok {
		return nil, 0, time.Time{}, false, nil
	}
	return n.ni.Snapshot(), n.gen, n.ts, true, nil
}

func (m *MemNodeStore) ListChangedSinceGen(_ context.Context, minGen int64, pageSize int32) ([]string, int64, error) {
	var names []string
	maxGen := minGen

	for k, v := range m.nodes {
		if v.gen > minGen {
			names = append(names, k)
			if v.gen > maxGen {
				maxGen = v.gen
			}
		}
	}

	sort.Slice(names, func(i, j int) bool {
		return m.nodes[names[i]].gen < m.nodes[names[j]].gen
	})

	if pageSize > 0 && int32(len(names)) > pageSize {
		names = names[:pageSize]
	}
	return names, maxGen, nil
}

func (m *MemNodeStore) Clear(_ context.Context) error {
	m.nodes = make(map[string]*memNode)
	return nil
}

func (m *MemNodeStore) ListLiveNames(_ context.Context, page int32) ([]string, error) {
	names := make([]string, 0, len(m.nodes))
	for k, v := range m.nodes {
		if v.ni != nil && v.ni.Node() != nil {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	if page > 0 && int32(len(names)) > page {
		names = names[:page]
	}
	return names, nil
}

//
// ---------------------------  DynamoDB adapter  --------------------------- //
//

type DDBNodeStore struct {
	ctx context.Context
	aws *nodeAWS
}

func NewDDBNodeStore(
	ctx context.Context,
	ddb *dynamodb.Client,
	table string,
	wipeOnStart bool,
) (*DDBNodeStore, error) {

	dao := newNodeAWS(table, ddb)
	if wipeOnStart {
		if err := dao.clear(ctx); err != nil {
			return nil, err
		}
	}
	return &DDBNodeStore{ctx: ctx, aws: dao}, nil
}

func (s *DDBNodeStore) Put(ctx context.Context, name string, ni *framework.NodeInfo, gen int64, ts time.Time) error {
	payload, err := marshalNodeInfo(ni)
	if err != nil {
		return err
	}
	live := ni.Node() != nil
	return s.aws.put(ctx, name, payload, gen, ts, live)
}

func (s *DDBNodeStore) GetByName(ctx context.Context, name string) (*framework.NodeInfo, int64, time.Time, bool, error) {
	pi, gen, tsMillis, ok, err := s.aws.get(ctx, name)
	if err != nil || !ok {
		return nil, 0, time.Time{}, ok, err
	}
	ni, err := unmarshalNodeInfo(pi)
	return ni, gen, time.UnixMilli(tsMillis), true, err
}

func (s *DDBNodeStore) ListChangedSinceGen(ctx context.Context, minGen int64, page int32) ([]string, int64, error) {
	return s.aws.listAfterGen(ctx, minGen, page)
}

func (s *DDBNodeStore) Clear(ctx context.Context) error {
	return s.aws.clear(ctx)
}

func (s *DDBNodeStore) ListLiveNames(ctx context.Context, page int32) ([]string, error) {
	return s.aws.listLiveNames(ctx, page)
}
