package awsstore

import (
	"context"
	"encoding/json"
	"fmt"
	"path"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

const (
	pkByNode = "nominatedByNode" // SK = "<node>/<uid>" → payload = podRef JSON
	pkByUID  = "nominatedByUID"  // SK = "<uid>"       → payload = nodeName bytes
)

type NominatedStore interface {
	Upsert(ctx context.Context, uid k8stypes.UID, node string, pr PodRef) error
	Put(ctx context.Context, uid k8stypes.UID, nodeName string, pr PodRef) error
	Delete(ctx context.Context, uid k8stypes.UID, nodeName string) error
	ListByNode(ctx context.Context, nodeName string) ([]PodRef, error)
	GetNodeForUID(ctx context.Context, uid k8stypes.UID) (string, bool, error)
	Clear(ctx context.Context) error
}

type DDBNominatedStore struct {
	ctx      context.Context
	ddb      *dynamodb.Client
	byNodePS *PartitionStore
	byUIDPS  *PartitionStore
}

func NewDDBNominatedStore(ctx context.Context, ddb *dynamodb.Client, wipe bool) (*DDBNominatedStore, error) {
	byNode, err := NewPartitionStore(ctx, ddb, PartitionStoreTableName, pkByNode, 0, wipe)
	if err != nil {
		return nil, err
	}
	byUID, err := NewPartitionStore(ctx, ddb, PartitionStoreTableName, pkByUID, 0, wipe)
	if err != nil {
		return nil, err
	}
	return &DDBNominatedStore{ctx: ctx, ddb: ddb, byNodePS: byNode, byUIDPS: byUID}, nil
}

func (s *DDBNominatedStore) Put(ctx context.Context, uid k8stypes.UID, node string, pr PodRef) error {
	key := path.Join(node, string(uid)) // "<node>/<uid>"
	b, _ := json.Marshal(wireNom{Pod: pr})
	if err := s.byNodePS.Put(ctx, key, b); err != nil {
		return err
	}
	return s.byUIDPS.Put(ctx, string(uid), []byte(node))
}

func (s *DDBNominatedStore) Delete(ctx context.Context, uid k8stypes.UID, node string) error {
	if node == "" {
		if b, _, ok, _ := s.byUIDPS.Get(ctx, string(uid)); ok {
			node = string(b)
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

func (s *DDBNominatedStore) GetNodeForUID(ctx context.Context, uid k8stypes.UID) (string, bool, error) {
	b, _, ok, err := s.byUIDPS.Get(ctx, string(uid))
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

func (s *DDBNominatedStore) Upsert(ctx context.Context, uid k8stypes.UID, node string, pr PodRef) error {
	const maxAttempts = 8
	uidKey := string(uid)

	payload, _ := json.Marshal(wireNom{Pod: pr})

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// 1) read current reverse mapping
		oldB, _, ok, err := s.byUIDPS.Get(ctx, uidKey)
		if err != nil {
			return err
		}
		oldNode := ""
		if ok {
			oldNode = string(oldB)
		}

		// 2) build txn
		items := make([]ddbtypes.TransactWriteItem, 0, 3)

		// delete old forward index if moving across nodes
		if oldNode != "" && oldNode != node {
			items = append(items, ddbtypes.TransactWriteItem{
				Delete: &ddbtypes.Delete{
					TableName: aws.String(s.byNodePS.table),
					Key: map[string]ddbtypes.AttributeValue{
						attrPK: &ddbtypes.AttributeValueMemberS{Value: s.byNodePS.pk},
						attrSK: &ddbtypes.AttributeValueMemberS{Value: path.Join(oldNode, uidKey)},
					},
				},
			})
		}

		// put new forward index
		items = append(items, ddbtypes.TransactWriteItem{
			Put: &ddbtypes.Put{
				TableName: aws.String(s.byNodePS.table),
				Item: map[string]ddbtypes.AttributeValue{
					attrPK: &ddbtypes.AttributeValueMemberS{Value: s.byNodePS.pk},
					attrSK: &ddbtypes.AttributeValueMemberS{Value: path.Join(node, uidKey)},
					attrPi: &ddbtypes.AttributeValueMemberB{Value: payload},
				},
			},
		})

		// reverse index: uid -> node (CAS on old value)
		cond := "attribute_not_exists(" + attrPi + ")"
		var eav map[string]ddbtypes.AttributeValue
		if oldNode != "" {
			cond += " OR " + attrPi + " = :old"
			eav = map[string]ddbtypes.AttributeValue{
				":old": &ddbtypes.AttributeValueMemberB{Value: []byte(oldNode)},
			}
		}

		items = append(items, ddbtypes.TransactWriteItem{
			Put: &ddbtypes.Put{
				TableName: aws.String(s.byUIDPS.table),
				Item: map[string]ddbtypes.AttributeValue{
					attrPK: &ddbtypes.AttributeValueMemberS{Value: s.byUIDPS.pk},
					attrSK: &ddbtypes.AttributeValueMemberS{Value: uidKey},
					// keep it BINARY to match PartitionStore.Get
					attrPi: &ddbtypes.AttributeValueMemberB{Value: []byte(node)},
				},
				ConditionExpression:       aws.String(cond),
				ExpressionAttributeValues: eav, // nil when no :old
			},
		})

		// 3) execute txn
		_, err = s.ddb.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
			TransactItems: items,
		})
		if err == nil {
			return nil
		}
		if isCCF(err) {
			continue
		} // retry on conflict
		return err
	}
	return fmt.Errorf("%w: Upsert nomination for %s -> %s", ErrConflict, uid, node)
}
