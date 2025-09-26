package awsstore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	nodeTablePK      = "PK"
	nodeTableSK      = "SK"
	nodeAttrPi       = "Pi"
	nodeAttrGen      = "Gen"
	nodeAttrTS       = "TS"
	nodePKByName     = "node#byName"
	nodePKLive       = "node#live"
	nodeGSIByGenName = "GSI_NodeByGen"
)

type nodeAWS struct {
	table string
	ddb   *dynamodb.Client
}

func isCCF(err error) bool {
	var ccf *types.ConditionalCheckFailedException
	if errors.As(err, &ccf) {
		return true
	}
	var tce *types.TransactionCanceledException
	if errors.As(err, &tce) {
		for _, r := range tce.CancellationReasons {
			if r.Code != nil && *r.Code == "ConditionalCheckFailed" {
				return true
			}
		}
	}
	return false
}

func newNodeAWS(table string, ddb *dynamodb.Client) *nodeAWS {
	return &nodeAWS{table: table, ddb: ddb}
}

func (n *nodeAWS) put(ctx context.Context, name string, payload []byte, gen int64, ts time.Time, live bool) error {
	newGen := gen + 1

	updateNode := types.TransactWriteItem{
		Update: &types.Update{
			TableName: aws.String(n.table),
			Key: map[string]types.AttributeValue{
				nodeTablePK: &types.AttributeValueMemberS{Value: nodePKByName},
				nodeTableSK: &types.AttributeValueMemberS{Value: name},
			},
			// Allow to create when Gen doesn't exist; otherwise require Gen == :exp
			ConditionExpression: aws.String(
				"attribute_not_exists(" + nodeAttrGen + ") OR " + nodeAttrGen + " = :exp",
			),
			UpdateExpression: aws.String(
				"SET " + nodeAttrPi + " = :pi, " + nodeAttrGen + " = :newGen, " + nodeAttrTS + " = :ts",
			),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pi":     &types.AttributeValueMemberB{Value: payload},
				":exp":    &types.AttributeValueMemberN{Value: strconv.FormatInt(gen, 10)},
				":newGen": &types.AttributeValueMemberN{Value: strconv.FormatInt(newGen, 10)},
				":ts":     &types.AttributeValueMemberN{Value: strconv.FormatInt(ts.UnixMilli(), 10)},
			},
		},
	}

	var liveOp types.TransactWriteItem
	if live {
		liveOp = types.TransactWriteItem{
			Put: &types.Put{
				TableName: aws.String(n.table),
				Item: map[string]types.AttributeValue{
					nodeTablePK: &types.AttributeValueMemberS{Value: nodePKLive},
					nodeTableSK: &types.AttributeValueMemberS{Value: name},
					nodeAttrTS:  &types.AttributeValueMemberN{Value: strconv.FormatInt(ts.UnixMilli(), 10)},
				},
			},
		}
	} else {
		liveOp = types.TransactWriteItem{
			Delete: &types.Delete{
				TableName: aws.String(n.table),
				Key: map[string]types.AttributeValue{
					nodeTablePK: &types.AttributeValueMemberS{Value: nodePKLive},
					nodeTableSK: &types.AttributeValueMemberS{Value: name},
				},
				// No condition: delete is idempotent even if not present
			},
		}
	}

	_, err := n.ddb.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{updateNode, liveOp},
	})
	if err != nil {
		if isCCF(err) {
			return fmt.Errorf("%w: node %q generation conflict (expected %d)", ErrConflict, name, gen)
		}
		return err
	}
	return nil
}

func (n *nodeAWS) get(ctx context.Context, name string) ([]byte, int64, int64, bool, error) {
	out, err := n.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(n.table),
		ConsistentRead: aws.Bool(true),
		Key: map[string]types.AttributeValue{
			nodeTablePK: &types.AttributeValueMemberS{Value: nodePKByName},
			nodeTableSK: &types.AttributeValueMemberS{Value: name},
		},
	})
	if err != nil || out.Item == nil {
		return nil, 0, 0, false, err
	}
	gen, _ := strconv.ParseInt(out.Item[nodeAttrGen].(*types.AttributeValueMemberN).Value, 10, 64)
	ts, _ := strconv.ParseInt(out.Item[nodeAttrTS].(*types.AttributeValueMemberN).Value, 10, 64)
	pi := out.Item[nodeAttrPi].(*types.AttributeValueMemberB).Value
	return pi, gen, ts, true, nil
}

func (n *nodeAWS) listAfterGen(ctx context.Context, minGen int64, page int32) ([]string, int64, error) {
	var (
		names  []string
		maxGen = minGen
		start  map[string]types.AttributeValue
		lim    = page
	)
	if lim <= 0 {
		lim = 100
	}

	for {
		q, err := n.ddb.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(n.table),
			IndexName:              aws.String(nodeGSIByGenName),
			KeyConditionExpression: aws.String(nodeTablePK + " = :pk AND " + nodeAttrGen + " > :g"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk": &types.AttributeValueMemberS{Value: nodePKByName},
				":g":  &types.AttributeValueMemberN{Value: strconv.FormatInt(minGen, 10)},
			},
			ScanIndexForward:  aws.Bool(true),
			ExclusiveStartKey: start,
			Limit:             aws.Int32(lim),
		})
		if err != nil {
			return nil, maxGen, err
		}

		for _, it := range q.Items {
			nameAttr, ok := it[nodeTableSK].(*types.AttributeValueMemberS)
			if !ok {
				continue
			}
			names = append(names, nameAttr.Value)

			if gAttr, ok := it[nodeAttrGen].(*types.AttributeValueMemberN); ok {
				if g, _ := strconv.ParseInt(gAttr.Value, 10, 64); g > maxGen {
					maxGen = g
				}
			}
			if page > 0 && int32(len(names)) >= page {
				return names, maxGen, nil
			}
		}

		if len(q.LastEvaluatedKey) == 0 {
			break
		}
		start = q.LastEvaluatedKey
	}
	return names, maxGen, nil
}

func (n *nodeAWS) listLiveNames(ctx context.Context, page int32) ([]string, error) {
	var (
		start map[string]types.AttributeValue
		lim   = page
		names []string
	)
	if lim <= 0 {
		lim = 100
	}
	for {
		out, err := n.ddb.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(n.table),
			KeyConditionExpression: aws.String(nodeTablePK + " = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk": &types.AttributeValueMemberS{Value: nodePKLive},
			},
			ScanIndexForward:     aws.Bool(true),
			ExclusiveStartKey:    start,
			Limit:                aws.Int32(lim),
			ProjectionExpression: aws.String(nodeTableSK),
		})
		if err != nil {
			return nil, err
		}
		for _, it := range out.Items {
			names = append(names, it[nodeTableSK].(*types.AttributeValueMemberS).Value)
		}
		if len(out.LastEvaluatedKey) == 0 {
			break
		}
		start = out.LastEvaluatedKey
	}
	return names, nil
}

func (n *nodeAWS) clear(ctx context.Context) error {
	for _, pk := range []string{nodePKByName, nodePKLive} {
		var start map[string]types.AttributeValue
		for {
			q, err := n.ddb.Query(ctx, &dynamodb.QueryInput{
				TableName:              aws.String(n.table),
				KeyConditionExpression: aws.String(nodeTablePK + " = :pk"),
				ExpressionAttributeValues: map[string]types.AttributeValue{
					":pk": &types.AttributeValueMemberS{Value: pk},
				},
				ProjectionExpression: aws.String(nodeTablePK + ", " + nodeTableSK),
				ExclusiveStartKey:    start,
			})
			if err != nil {
				return err
			}
			if len(q.Items) == 0 {
				break
			}
			for i := 0; i < len(q.Items); i += 25 {
				end := i + 25
				if end > len(q.Items) {
					end = len(q.Items)
				}
				chunk := q.Items[i:end]
				reqs := make([]types.WriteRequest, 0, len(chunk))
				for _, it := range chunk {
					reqs = append(reqs, types.WriteRequest{DeleteRequest: &types.DeleteRequest{Key: it}})
				}
				if _, err := n.ddb.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
					RequestItems: map[string][]types.WriteRequest{n.table: reqs},
				}); err != nil {
					return err
				}
			}
			start = q.LastEvaluatedKey
		}
	}
	return nil
}

func (n *nodeAWS) keyFor(name string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		nodeTablePK: &types.AttributeValueMemberS{Value: nodePKByName},
		nodeTableSK: &types.AttributeValueMemberS{Value: name},
	}
}
