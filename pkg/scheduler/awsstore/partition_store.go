package awsstore

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"strconv"
	"time"
)

const (
	attrPK   = "PK"
	attrSK   = "SK"
	attrPi   = "Pi"
	attrTTL  = "TTL"
	maxBatch = 25
)

type PartitionStore struct {
	table       string
	pk          string
	ttl         time.Duration
	ddb         *dynamodb.Client
	wipeOnStart bool
}

func NewPartitionStore(
	ctx context.Context,
	ddb *dynamodb.Client,
	table string,
	pk string,
	ttl time.Duration,
	wipeOnStart bool,
) (*PartitionStore, error) {
	// Use the general EnsurePartitionStoreTable function to ensure the table exists.
	ps := &PartitionStore{table: table, pk: pk, ttl: ttl, ddb: ddb, wipeOnStart: wipeOnStart}

	if wipeOnStart {
		if err := ps.Clear(ctx); err != nil {
			return nil, err
		}
	}
	return ps, nil
}

func (ps *PartitionStore) Put(ctx context.Context, sk string, payload []byte) error {
	item := map[string]types.AttributeValue{
		attrPK: &types.AttributeValueMemberS{Value: ps.pk},
		attrSK: &types.AttributeValueMemberS{Value: sk},
		attrPi: &types.AttributeValueMemberB{Value: payload},
	}
	if ps.ttl > 0 {
		item[attrTTL] = &types.AttributeValueMemberN{Value: strconv.FormatInt(time.Now().Add(ps.ttl).Unix(), 10)}
	}
	_, err := ps.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &ps.table,
		Item:      item,
	})
	return err
}

func (ps *PartitionStore) Get(ctx context.Context, sk string) ([]byte, bool, error) {
	out, err := ps.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &ps.table,
		ConsistentRead: aws.Bool(true),
		Key: map[string]types.AttributeValue{
			attrPK: &types.AttributeValueMemberS{Value: ps.pk},
			attrSK: &types.AttributeValueMemberS{Value: sk},
		},
	})
	if err != nil {
		return nil, false, err
	}
	if out.Item == nil {
		return nil, false, nil
	}
	pi, ok := out.Item[attrPi].(*types.AttributeValueMemberB)
	if !ok {
		return nil, false, fmt.Errorf("Pi missing")
	}
	return pi.Value, true, nil
}

func (ps *PartitionStore) Delete(ctx context.Context, sk string) error {
	_, err := ps.ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: &ps.table,
		Key: map[string]types.AttributeValue{
			attrPK: &types.AttributeValueMemberS{Value: ps.pk},
			attrSK: &types.AttributeValueMemberS{Value: sk},
		},
	})
	return err
}

func (ps *PartitionStore) List(ctx context.Context) (map[string][]byte, error) {
	var start map[string]types.AttributeValue
	outMap := make(map[string][]byte)
	for {
		out, err := ps.ddb.Query(ctx, &dynamodb.QueryInput{
			TableName:              &ps.table,
			KeyConditionExpression: aws.String("PK = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk": &types.AttributeValueMemberS{Value: ps.pk},
			},
			ProjectionExpression: aws.String("SK, Pi"),
			ExclusiveStartKey:    start,
		})
		if err != nil {
			return nil, err
		}
		for _, it := range out.Items {
			sk := it[attrSK].(*types.AttributeValueMemberS).Value
			pi := it[attrPi].(*types.AttributeValueMemberB).Value
			outMap[sk] = pi
		}
		if len(out.LastEvaluatedKey) == 0 {
			return outMap, nil
		}
		start = out.LastEvaluatedKey
	}
}

func (ps *PartitionStore) Clear(ctx context.Context) error {
	var start map[string]types.AttributeValue
	for {
		out, err := ps.ddb.Query(ctx, &dynamodb.QueryInput{
			TableName:              &ps.table,
			KeyConditionExpression: aws.String("PK = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk": &types.AttributeValueMemberS{Value: ps.pk},
			},
			ProjectionExpression: aws.String("PK, SK"),
			ExclusiveStartKey:    start,
		})
		if err != nil {
			return err
		}
		if len(out.Items) == 0 {
			return nil
		}

		for i := 0; i < len(out.Items); i += maxBatch {
			end := i + maxBatch
			if end > len(out.Items) {
				end = len(out.Items)
			}
			chunk := out.Items[i:end]
			reqs := make([]types.WriteRequest, 0, len(chunk))
			for _, it := range chunk {
				reqs = append(reqs, types.WriteRequest{
					DeleteRequest: &types.DeleteRequest{Key: it},
				})
			}
			toWrite := map[string][]types.WriteRequest{ps.table: reqs}
			for len(toWrite[ps.table]) > 0 {
				bwo, err := ps.ddb.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
					RequestItems: toWrite,
				})
				if err != nil {
					return err
				}
				toWrite = bwo.UnprocessedItems
			}
		}

		start = out.LastEvaluatedKey
	}
}

// ListByPrefix returns SK->payload for items with ps.pk and SK that begins_with(prefix).
func (ps *PartitionStore) ListByPrefix(ctx context.Context, prefix string) (map[string][]byte, error) {
	out := make(map[string][]byte)
	var start map[string]types.AttributeValue

	for {
		q, err := ps.ddb.Query(ctx, &dynamodb.QueryInput{
			TableName:              &ps.table,
			KeyConditionExpression: aws.String(attrPK + " = :pk AND begins_with(" + attrSK + ", :pfx)"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk":  &types.AttributeValueMemberS{Value: ps.pk},
				":pfx": &types.AttributeValueMemberS{Value: prefix},
			},
			ProjectionExpression: aws.String(attrSK + ", " + attrPi),
			ExclusiveStartKey:    start,
		})
		if err != nil {
			return nil, err
		}
		for _, it := range q.Items {
			sk := it[attrSK].(*types.AttributeValueMemberS).Value
			pi := it[attrPi].(*types.AttributeValueMemberB).Value
			out[sk] = pi
		}
		if len(q.LastEvaluatedKey) == 0 {
			return out, nil
		}
		start = q.LastEvaluatedKey
	}
}
