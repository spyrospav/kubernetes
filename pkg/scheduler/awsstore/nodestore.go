package awsstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

type NodeStore interface {
	Put(ctx context.Context, name string, ni *framework.NodeInfo, gen int64, ts time.Time) error
	GetByName(ctx context.Context, name string) (*framework.NodeInfo, int64, time.Time, bool, error)
	ListChangedSinceGen(ctx context.Context, minGen int64, pageSize int32) (names []string, maxGen int64, err error)
	Clear(ctx context.Context) error
	ListLiveNames(ctx context.Context, pageSize int32) ([]string, error)
	GetManyByNames(ctx context.Context, names []string) (map[string]*framework.NodeInfo, error)
}

const maxBatchGet = 100 // DynamoDB per-table limit

var ErrConflict = errors.New("nodestore: conflict")

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

func (s *DDBNodeStore) GetManyByNames(ctx context.Context, names []string) (map[string]*framework.NodeInfo, error) {
	if len(names) == 0 {
		return map[string]*framework.NodeInfo{}, nil
	}
	out := make(map[string]*framework.NodeInfo, len(names))

	// Tiny exponential-ish backoff for UnprocessedKeys
	backoff := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}

	for i := 0; i < len(names); i += maxBatchGet {
		end := i + maxBatchGet
		if end > len(names) {
			end = len(names)
		}
		chunk := names[i:end]

		// Build keys for this chunk
		keys := make([]map[string]ddb.AttributeValue, 0, len(chunk))
		for _, name := range chunk {
			keys = append(keys, s.aws.keyFor(name))
		}

		req := &dynamodb.BatchGetItemInput{
			RequestItems: map[string]ddb.KeysAndAttributes{
				s.aws.table: {
					Keys:                 keys,
					ProjectionExpression: aws.String(nodeAttrPi), // payload only
					ConsistentRead:       aws.Bool(false),
				},
			},
		}

		var (
			resp *dynamodb.BatchGetItemOutput
			err  error
		)
		for attempt := 0; ; attempt++ {
			resp, err = s.aws.ddb.BatchGetItem(ctx, req)
			if err != nil {
				return nil, err
			}

			// Parse what we got
			items := resp.Responses[s.aws.table]
			for _, it := range items {
				// Prefer binary payload
				if b, ok := it[nodeAttrPi].(*ddb.AttributeValueMemberB); ok {
					if ni, uerr := unmarshalNodeInfo(b.Value); uerr == nil && ni != nil && ni.Node() != nil {
						out[ni.Node().Name] = ni
					}
					continue
				}
				// Allow string payload just in case schema differs
				if sb, ok := it[nodeAttrPi].(*ddb.AttributeValueMemberS); ok {
					if ni, uerr := unmarshalNodeInfo([]byte(sb.Value)); uerr == nil && ni != nil && ni.Node() != nil {
						out[ni.Node().Name] = ni
					}
				}
			}

			// Any unprocessed keys left? Retry just those.
			if ka, ok := resp.UnprocessedKeys[s.aws.table]; !ok || len(ka.Keys) == 0 {
				break
			}
			req.RequestItems = resp.UnprocessedKeys

			if attempt < len(backoff) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(backoff[attempt]):
				}
				continue
			}
			return nil, fmt.Errorf("batch get: unprocessed keys remain after retries")
		}
	}
	return out, nil
}
