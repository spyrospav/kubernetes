package awsstore

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	attrPK      = "PK"
	attrSK      = "SK"
	attrPi      = "Pi"
	attrTTL     = "TTL"
	attrVer     = "Ver"
	attrAssumed = "Assumed"
	attrTS      = "TS"
	attrCO      = "CO"
	attrCU      = "CU"
	maxBatch    = 25
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
	upd := "SET " + attrPi + " = :pl"
	eav := map[string]types.AttributeValue{
		":pl": &types.AttributeValueMemberB{Value: payload},
	}
	if ps.ttl > 0 {
		upd += ", " + attrTTL + " = :ttl"
		eav[":ttl"] = &types.AttributeValueMemberN{
			Value: strconv.FormatInt(time.Now().Add(ps.ttl).Unix(), 10),
		}
	}

	_, err := ps.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: &ps.table,
		Key: map[string]types.AttributeValue{
			attrPK: &types.AttributeValueMemberS{Value: ps.pk},
			attrSK: &types.AttributeValueMemberS{Value: sk},
		},
		UpdateExpression:          aws.String(upd),
		ExpressionAttributeValues: eav,
	})
	return err
}

func (ps *PartitionStore) Get(ctx context.Context, sk string) ([]byte, int64, bool, error) {
	out, err := ps.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &ps.table,
		ConsistentRead: aws.Bool(true),
		Key: map[string]types.AttributeValue{
			attrPK: &types.AttributeValueMemberS{Value: ps.pk},
			attrSK: &types.AttributeValueMemberS{Value: sk},
		},
	})
	if err != nil {
		return nil, 0, false, err
	}
	if out.Item == nil {
		return nil, 0, false, nil
	}

	pi, ok := out.Item[attrPi].(*types.AttributeValueMemberB)
	if !ok {
		return nil, 0, false, fmt.Errorf("Pi missing")
	}

	vn, ok := out.Item[attrVer].(*types.AttributeValueMemberN)
	if !ok {
		return nil, 0, false, fmt.Errorf("Ver missing")
	}
	ver, err := strconv.ParseInt(vn.Value, 10, 64)
	if err != nil {
		return nil, 0, false, fmt.Errorf("bad Ver: %w", err)
	}

	return pi.Value, ver, true, nil
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

func (ps *PartitionStore) PutCreate(ctx context.Context, sk string, payload []byte, assumed bool) error {
	now := time.Now().UnixMilli()

	upd := "SET " + attrPi + " = :pl, " + attrAssumed + " = :as, " +
		attrTS + " = :ts, " + attrVer + " = :one"
	eav := map[string]types.AttributeValue{
		":pl":  &types.AttributeValueMemberB{Value: payload},
		":as":  &types.AttributeValueMemberBOOL{Value: assumed},
		":ts":  &types.AttributeValueMemberN{Value: strconv.FormatInt(now, 10)},
		":one": &types.AttributeValueMemberN{Value: "1"},
	}
	if ps.ttl > 0 {
		upd += ", " + attrTTL + " = :ttl"
		eav[":ttl"] = &types.AttributeValueMemberN{
			Value: strconv.FormatInt(time.Now().Add(ps.ttl).Unix(), 10),
		}
	}

	_, err := ps.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &ps.table,
		Key:                       map[string]types.AttributeValue{attrPK: &types.AttributeValueMemberS{Value: ps.pk}, attrSK: &types.AttributeValueMemberS{Value: sk}},
		ConditionExpression:       aws.String("attribute_not_exists(" + attrVer + ")"),
		UpdateExpression:          aws.String(upd),
		ExpressionAttributeValues: eav,
	})
	if err != nil && isCCF(err) {
		return fmt.Errorf("%w: podstate %q already exists", ErrConflict, sk)
	}
	return err
}

func (ps *PartitionStore) PutUpdateCAS(ctx context.Context, sk string, payload []byte, assumed bool, expectedVer int64) error {
	now := time.Now().UnixMilli()

	upd := "SET " + attrPi + " = :pl, " + attrAssumed + " = :as, " +
		attrTS + " = :ts, " + attrVer + " = :new"
	eav := map[string]types.AttributeValue{
		":pl":  &types.AttributeValueMemberB{Value: payload},
		":as":  &types.AttributeValueMemberBOOL{Value: assumed},
		":ts":  &types.AttributeValueMemberN{Value: strconv.FormatInt(now, 10)},
		":exp": &types.AttributeValueMemberN{Value: strconv.FormatInt(expectedVer, 10)},
		":new": &types.AttributeValueMemberN{Value: strconv.FormatInt(expectedVer+1, 10)},
	}
	if ps.ttl > 0 {
		upd += ", " + attrTTL + " = :ttl"
		eav[":ttl"] = &types.AttributeValueMemberN{
			Value: strconv.FormatInt(time.Now().Add(ps.ttl).Unix(), 10),
		}
	}

	cond := "attribute_exists(" + attrVer + ") AND " + attrVer + " = :exp"

	_, err := ps.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &ps.table,
		Key:                       map[string]types.AttributeValue{attrPK: &types.AttributeValueMemberS{Value: ps.pk}, attrSK: &types.AttributeValueMemberS{Value: sk}},
		ConditionExpression:       aws.String(cond),
		UpdateExpression:          aws.String(upd),
		ExpressionAttributeValues: eav,
	})
	if err != nil && isCCF(err) {
		return fmt.Errorf("%w: podstate %q version conflict (expected %d)", ErrConflict, sk, expectedVer)
	}
	return err
}

func (ps *PartitionStore) PutUpsertCAS(ctx context.Context, sk string, payload []byte, assumed bool, expectedVer int64) error {
	now := time.Now().UnixMilli()

	// Ver := if_not_exists(Ver, 0) + 1
	upd := "SET " + attrPi + " = :pl, " + attrAssumed + " = :as, " + attrTS + " = :ts, " +
		attrVer + " = if_not_exists(" + attrVer + ", :zero) + :one"
	eav := map[string]types.AttributeValue{
		":pl":   &types.AttributeValueMemberB{Value: payload},
		":as":   &types.AttributeValueMemberBOOL{Value: assumed},
		":ts":   &types.AttributeValueMemberN{Value: strconv.FormatInt(now, 10)},
		":exp":  &types.AttributeValueMemberN{Value: strconv.FormatInt(expectedVer, 10)},
		":zero": &types.AttributeValueMemberN{Value: "0"},
		":one":  &types.AttributeValueMemberN{Value: "1"},
	}
	if ps.ttl > 0 {
		upd += ", " + attrTTL + " = :ttl"
		eav[":ttl"] = &types.AttributeValueMemberN{
			Value: strconv.FormatInt(time.Now().Add(ps.ttl).Unix(), 10),
		}
	}

	// Allow create OR CAS update
	cond := "attribute_not_exists(" + attrVer + ") OR " + attrVer + " = :exp"

	_, err := ps.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &ps.table,
		Key:                       map[string]types.AttributeValue{attrPK: &types.AttributeValueMemberS{Value: ps.pk}, attrSK: &types.AttributeValueMemberS{Value: sk}},
		ConditionExpression:       aws.String(cond),
		UpdateExpression:          aws.String(upd),
		ExpressionAttributeValues: eav,
	})
	if err != nil && isCCF(err) {
		return fmt.Errorf("%w: podstate %q version conflict (expected %d)", ErrConflict, sk, expectedVer)
	}
	return err
}

// DeleteIfVersion deletes only if current Ver equals expectedVer (or item doesn’t exist).
func (ps *PartitionStore) DeleteIfVersion(ctx context.Context, sk string, expectedVer int64) error {
	_, err := ps.ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: &ps.table,
		Key: map[string]types.AttributeValue{
			attrPK: &types.AttributeValueMemberS{Value: ps.pk},
			attrSK: &types.AttributeValueMemberS{Value: sk},
		},
		ConditionExpression: aws.String(
			"attribute_not_exists(" + attrVer + ") OR " + attrVer + " = :exp",
		),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":exp": &types.AttributeValueMemberN{Value: strconv.FormatInt(expectedVer, 10)},
		},
	})
	if err != nil && isCCF(err) {
		return fmt.Errorf("%w: podstate %q version conflict (expected %d)", ErrConflict, sk, expectedVer)
	}
	return err
}

// ClaimNext tries to claim (lease) one item for the given owner.
func (ps *PartitionStore) ClaimNext(
	ctx context.Context,
	owner string,
	lease time.Duration,
	startAfterSK string,
	page int32,
) (string, []byte, bool, error) {

	if page <= 0 {
		page = 50
	}
	now := time.Now().UnixMilli()
	until := now + lease.Milliseconds()

	// Build ExclusiveStartKey if caller passed a starting SK
	var esk map[string]types.AttributeValue
	if startAfterSK != "" {
		esk = map[string]types.AttributeValue{
			attrPK: &types.AttributeValueMemberS{Value: ps.pk},
			attrSK: &types.AttributeValueMemberS{Value: startAfterSK},
		}
	}

	// Query a window of candidates within this partition
	q, err := ps.ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(ps.table),
		KeyConditionExpression: aws.String(attrPK + " = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: ps.pk},
		},
		ProjectionExpression: aws.String(attrSK), // we only need SK to attempt a claim
		ExclusiveStartKey:    esk,
		ScanIndexForward:     aws.Bool(true),
		Limit:                aws.Int32(page),
	})
	if err != nil {
		return "", nil, false, err
	}
	if len(q.Items) == 0 {
		return "", nil, false, nil
	}

	// Try to claim one by one in this page
	for _, it := range q.Items {
		sk := it[attrSK].(*types.AttributeValueMemberS).Value

		// Conditional claim: set CO/CU if (no claim) OR (same owner) OR (expired)
		upd := "SET " + attrCO + " = :me, " + attrCU + " = :until"
		cond := "attribute_not_exists(" + attrCO + ") OR " + attrCO + " = :me OR " + attrCU + " < :now"

		eav := map[string]types.AttributeValue{
			":me":    &types.AttributeValueMemberS{Value: owner},
			":now":   &types.AttributeValueMemberN{Value: strconv.FormatInt(now, 10)},
			":until": &types.AttributeValueMemberN{Value: strconv.FormatInt(until, 10)},
		}

		// If you set a TTL for this partition, keep honoring it
		if ps.ttl > 0 {
			// No need to touch TTL for claim; payload TTL is managed by Put/PutCAS.
		}

		// We want the payload after the claim succeeds.
		out, err := ps.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 aws.String(ps.table),
			Key:                       map[string]types.AttributeValue{attrPK: &types.AttributeValueMemberS{Value: ps.pk}, attrSK: &types.AttributeValueMemberS{Value: sk}},
			ConditionExpression:       aws.String(cond),
			UpdateExpression:          aws.String(upd),
			ExpressionAttributeValues: eav,
			ReturnValues:              types.ReturnValueAllNew,
		})
		if err != nil {
			// Someone else holds a valid lease → try next candidate
			if isCCF(err) {
				continue
			}
			return "", nil, false, err
		}

		// Claimed successfully; fetch payload
		pi, ok := out.Attributes[attrPi].(*types.AttributeValueMemberB)
		if !ok {
			// If payload is unexpectedly missing, release claim to avoid poison item
			_ = ps.ReleaseClaim(ctx, sk, owner)
			return "", nil, false, fmt.Errorf("payload missing for %s", sk)
		}
		return sk, pi.Value, true, nil
	}
	// No claimable item in this page
	return "", nil, false, nil
}

// ReleaseClaim removes the lease if the caller owns it.
func (ps *PartitionStore) ReleaseClaim(ctx context.Context, sk, owner string) error {
	_, err := ps.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(ps.table),
		Key: map[string]types.AttributeValue{
			attrPK: &types.AttributeValueMemberS{Value: ps.pk},
			attrSK: &types.AttributeValueMemberS{Value: sk},
		},
		ConditionExpression: aws.String(attrCO + " = :me"),
		UpdateExpression:    aws.String("REMOVE " + attrCO + ", " + attrCU),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":me": &types.AttributeValueMemberS{Value: owner},
		},
	})
	if err != nil && isCCF(err) {
		return fmt.Errorf("%w: release claim mismatch for %s", ErrConflict, sk)
	}
	return err
}

// DeleteIfClaimed deletes the item only if the caller owns the lease.
// If the item is already gone, we treat it as success.
func (ps *PartitionStore) DeleteIfClaimed(ctx context.Context, sk, owner string) error {
	_, err := ps.ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(ps.table),
		Key: map[string]types.AttributeValue{
			attrPK: &types.AttributeValueMemberS{Value: ps.pk},
			attrSK: &types.AttributeValueMemberS{Value: sk},
		},
		ConditionExpression: aws.String(attrCO + " = :me"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":me": &types.AttributeValueMemberS{Value: owner},
		},
	})
	if err != nil && isCCF(err) {
		return fmt.Errorf("%w: delete claim mismatch for %s", ErrConflict, sk)
	}
	return err
}
