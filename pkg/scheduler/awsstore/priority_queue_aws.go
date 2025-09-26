package awsstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

var rnd = rand.New(rand.NewSource(time.Now().UnixNano()))

const (
	BackendDynamoDB Backend = "dynamodb"
	BackendSQS      Backend = "sqs"

	normalMaxPriority   = int32(1_000_000_000)
	criticalPriorityVal = int32(2_000_000_000)
	systemPriorityVal   = int32(2_000_001_000)
	defaultBuckets      = 5

	milliFactor int64 = 256
	maxPrBucket       = 255
	uniqRange         = 256
	maxSuffix         = maxPrBucket*uniqRange + uniqRange - 1 // 65 535

	AttrTS       = "TS"      // numeric sort attribute used in the GSI
	IndexTSName  = "TSIndex" // GSI name
	AttrPriority = "Priority"
)

// Backend enumerates the storage services your queue supports.
type Backend string

// BackoffTimeFunc is a function type that calculates the backoff time for a pod.
type BackoffTimeFunc func(*framework.QueuedPodInfo) time.Time

// LessFunc is a function type that compares two QueuedPodInfo objects.
type LessFunc func(*framework.QueuedPodInfo, *framework.QueuedPodInfo) bool

// Config carries the runtime settings for a queue instance.
type Config struct {
	Backend   Backend // dynamodb or sqs
	TableName string  // DynamoDB table
	QueueID   string  // "activeQ", "backoffQ", … — becomes the PK

	QueuePrefix string // SQS queue prefix; used to create bucket queues
	NumBuckets  int    // optional; default = 5

	GetBackoffTime BackoffTimeFunc
	LessFunc       LessFunc
	PriorityAware  bool
}

// PriorityQueueAWS represents a remotely-backed priority queue.
type PriorityQueueAWS struct {
	cfg Config

	// DynamoDB Configuration
	dynamoClient *dynamodb.Client // non-nil when cfg.Backend == BackendDynamoDB

	// SQS Configuration
	sqsClient   *sqs.Client // non-nil when cfg.Backend == BackendSQS
	bucketURLs  []string    // len = NumBuckets
	criticalURL string
	systemURL   string
	bucketSize  int32 // = normalMaxPriority / NumBuckets

	wipeOnStart bool
}

// NewPriorityQueueAWS creates a new PriorityQueueAWS instance based on the provided configuration.
func NewPriorityQueueAWS(
	ctx context.Context,
	awsCfg aws.Config,
	cfg Config,
	ddbOpts []func(*dynamodb.Options),
	sqsOpts []func(*sqs.Options),
	wipeOnStart bool,
) (*PriorityQueueAWS, error) {

	pq := &PriorityQueueAWS{cfg: cfg}

	switch cfg.Backend {
	case BackendDynamoDB:
		if cfg.TableName == "" {
			return nil, fmt.Errorf("TableName must be set for DynamoDB backend")
		}
		pq.dynamoClient = dynamodb.NewFromConfig(awsCfg, ddbOpts...)
		if err := pq.ensureDynamoTable(ctx, pq.dynamoClient, cfg.TableName); err != nil {
			return nil, err
		}
		// clear the table on startup
		if wipeOnStart {
			if err := pq.Clear(ctx); err != nil {
				return nil, fmt.Errorf("failed to clear DynamoDB table %q: %w", cfg.TableName, err)
			}
		}
	case BackendSQS:
		pq.sqsClient = sqs.NewFromConfig(awsCfg, sqsOpts...)
		if err := pq.ensureSQSQueues(ctx); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported backend %q", cfg.Backend)
	}

	return pq, nil
}

// AddOrUpdate enqueues a pod into the priority queue, updating it if it already exists.
func (q *PriorityQueueAWS) AddOrUpdate(ctx context.Context, pInfo *framework.QueuedPodInfo) error {
	switch q.cfg.Backend {
	case BackendDynamoDB:
		return q.addOrUpdateDynamo(ctx, pInfo)
	case BackendSQS:
		return q.addSQS(ctx, pInfo)
	default:
		return fmt.Errorf("backend %s not implemented", q.cfg.Backend)
	}
}

// Pop dequeues the highest-priority pod from the queue.
func (q *PriorityQueueAWS) Pop(ctx context.Context) (*framework.QueuedPodInfo, error) {
	switch q.cfg.Backend {
	case BackendDynamoDB:
		if q.isBackoffQueue() {
			return q.popBackoffDynamo(ctx)
		}
		return q.popActiveDynamo(ctx)
	case BackendSQS:
		return q.popSQS(ctx)
	default:
		return nil, fmt.Errorf("backend %s not implemented", q.cfg.Backend)
	}
}

// Peek returns the highest-priority pod without removing it from the queue.
func (q *PriorityQueueAWS) Peek(ctx context.Context) (*framework.QueuedPodInfo, error) {
	switch q.cfg.Backend {
	case BackendDynamoDB:
		if q.isBackoffQueue() {
			return q.peekBackoffDynamo(ctx)
		}
		return q.peekActiveDynamo(ctx)
	case BackendSQS:
		return q.peekSQS(ctx)
	default:
		return nil, fmt.Errorf("backend %s not implemented", q.cfg.Backend)
	}
}

// Delete attempts to remove a pod from the queue by its UID.
func (q *PriorityQueueAWS) Delete(ctx context.Context, p *framework.QueuedPodInfo) error {
	if p == nil || p.Pod == nil {
		return nil
	}
	switch q.cfg.Backend {
	case BackendDynamoDB:
		_, err := q.dynamoClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String(q.cfg.TableName),
			Key: map[string]types.AttributeValue{
				"PK": &types.AttributeValueMemberS{Value: q.pk()},
				"SK": &types.AttributeValueMemberS{Value: string(p.Pod.UID)},
			},
		})
		return err
	default: // SQS – not supported
		return nil
	}
}

// List returns all pods currently in the queue.
func (q *PriorityQueueAWS) List(ctx context.Context) ([]*framework.QueuedPodInfo, error) {
	switch q.cfg.Backend {
	case BackendDynamoDB:
		out, err := q.dynamoClient.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(q.cfg.TableName),
			KeyConditionExpression: aws.String("PK = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk": &types.AttributeValueMemberS{Value: q.pk()},
			},
			ConsistentRead: aws.Bool(true),
		})
		if err != nil {
			return nil, err
		}
		pods := make([]*framework.QueuedPodInfo, 0, len(out.Items))
		for _, itm := range out.Items {
			payload := itm["Payload"].(*types.AttributeValueMemberB).Value
			if pi, err := UnmarshalQueuedPodInfo(payload); err == nil {
				pods = append(pods, pi)
			} else {
				klog.Error(err, "Failed to unmarshal pod info", "item", itm)
			}
		}
		return pods, nil
	default:
		return nil, nil
	}
}

// Count returns the number of queued pods.
func (q *PriorityQueueAWS) Count(ctx context.Context) (int, error) {
	switch q.cfg.Backend {
	case BackendDynamoDB:
		out, err := q.dynamoClient.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(q.cfg.TableName),
			KeyConditionExpression: aws.String("PK = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk": &types.AttributeValueMemberS{Value: q.pk()},
			},
			Select:         types.SelectCount,
			ConsistentRead: aws.Bool(true),
		})
		if err != nil {
			return 0, err
		}
		return int(out.Count), nil
	case BackendSQS:
		return 0, fmt.Errorf("not implemented: counting SQS queue length")
	default:
		return 0, fmt.Errorf("unsupported backend %q", q.cfg.Backend)
	}
}

// Clear removes all pods from the queue.
func (q *PriorityQueueAWS) Clear(ctx context.Context) error {
	switch q.cfg.Backend {
	case BackendDynamoDB:
		pk := q.pk()
		var last map[string]types.AttributeValue
		for {
			out, err := q.dynamoClient.Query(ctx, &dynamodb.QueryInput{
				TableName:              aws.String(q.cfg.TableName),
				KeyConditionExpression: aws.String("PK = :pk"),
				ExpressionAttributeValues: map[string]types.AttributeValue{
					":pk": &types.AttributeValueMemberS{Value: pk},
				},
				ProjectionExpression: aws.String("PK, SK"),
				ExclusiveStartKey:    last,
			})
			if err != nil {
				return err
			}
			if len(out.Items) == 0 {
				return nil
			}
			reqs := make([]types.WriteRequest, 0, len(out.Items))
			for _, it := range out.Items {
				reqs = append(reqs, types.WriteRequest{
					DeleteRequest: &types.DeleteRequest{Key: it},
				})
			}
			_, err = q.dynamoClient.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
				RequestItems: map[string][]types.WriteRequest{q.cfg.TableName: reqs},
			})
			if err != nil {
				return err
			}
			last = out.LastEvaluatedKey
		}
	default:
		return fmt.Errorf("unsupported backend %q for Clear operation", q.cfg.Backend)
	}
}

// ---------------------------  DYNAMO DB  --------------------------- //

func (q *PriorityQueueAWS) addOrUpdateDynamo(ctx context.Context, pInfo *framework.QueuedPodInfo) error {
	if pInfo == nil || pInfo.PodInfo == nil || pInfo.Pod == nil {
		return fmt.Errorf("nil pod")
	}

	item, err := q.prepareDynamoItem(pInfo)
	if err != nil {
		return err
	}

	// 1. try conditional insert ---------------------------------------------
	_, err = q.dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(q.cfg.TableName),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(SK)"),
	})
	if err == nil {
		return nil // new pod inserted
	}

	var ccfe *types.ConditionalCheckFailedException
	if !errors.As(err, &ccfe) {
		return err // real dynamo error
	}

	klog.Infof("Pod %s already exists in queue %s, updating it", pInfo.Pod.UID, q.cfg.QueueID)

	// 2. simple UPDATE ------------------------------
	_, err = q.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(q.cfg.TableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: q.pk()},
			"SK": &types.AttributeValueMemberS{Value: string(pInfo.Pod.UID)},
		},
		UpdateExpression: aws.String(`
            SET TS        = :ts,
                Priority  = :pr,
                Payload   = :pl,
                #tsmp     = :tsmp,
                Attempts  = :att
        `),
		ExpressionAttributeNames: map[string]string{
			"#tsmp": "Timestamp", // <-- alias reserved word
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":ts":   item[AttrTS],
			":pr":   item[AttrPriority],
			":pl":   item["Payload"],
			":tsmp": item["Timestamp"],
			":att":  item["Attempts"],
		},
	})
	return err
}

func (q *PriorityQueueAWS) popBackoffDynamo(ctx context.Context) (*framework.QueuedPodInfo, error) {
	if q.dynamoClient == nil {
		return nil, fmt.Errorf("dynamo client not initialised")
	}

	// For backoff queues with custom sorting (e.g., with priority),
	// we need to fetch multiple items and apply the comparison function
	// By default, not consider the priority case.
	if q.isBackoffQueue() && q.cfg.PriorityAware {
		return q.popBackoffWithPriority(ctx)
	}

	// Simple backoff queue - just pop the first item with completed backoff
	return q.popBackoff(ctx)
}

func (q *PriorityQueueAWS) popActiveDynamo(ctx context.Context) (*framework.QueuedPodInfo, error) {
	if q.dynamoClient == nil {
		return nil, fmt.Errorf("dynamo client not initialised")
	}

	for spin := 0; ; spin++ {
		// fast path -- eventually-consistent & cheap
		qout, err := q.dynamoClient.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(q.cfg.TableName),
			IndexName:              aws.String(IndexTSName),
			KeyConditionExpression: aws.String("PK = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk": &types.AttributeValueMemberS{Value: q.pk()},
			},
			Limit:            aws.Int32(1),
			ScanIndexForward: aws.Bool(true),
			ConsistentRead:   aws.Bool(false),
		})
		if err != nil {
			klog.Error(err, "Unable to query table for popping", "table", q.cfg.TableName)
			return nil, err
		}
		if len(qout.Items) == 0 {
			// Double-check with a strongly consistent COUNT.
			n, err := q.Count(ctx)
			if err != nil {
				return nil, err
			}
			if n == 0 {
				klog.Infof("Popping: %d items in queue %s", n, q.cfg.QueueID)
				return nil, nil
			}
			// Queue is not empty → GSI replica hasn’t caught up yet.
			// Light back-off before retrying.
			klog.Infof("Queue %s is not empty, but GSI replica is stale; waiting for %d ms",
				q.cfg.QueueID, spin+1)
			time.Sleep(time.Duration(spin+1) * 2 * time.Millisecond)
			continue
		}

		uid := qout.Items[0]["SK"].(*types.AttributeValueMemberS).Value
		pi, err := q.deleteByPodUID(ctx, uid)
		if err != nil {
			var ccfe *types.ConditionalCheckFailedException
			if errors.As(err, &ccfe) {
				// Someone else raced us; retry.
				continue
			}
			klog.Error(err, "Unable to delete item from queue", "queue", q.cfg.QueueID, "uid", uid)
			return nil, err
		}
		if pi == nil {
			klog.Error("Deleted item was nil", "queue", q.cfg.QueueID, "uid", uid)
			continue // extremely unlikely, but be safe.
		}
		return pi, nil
	}
}

func (q *PriorityQueueAWS) peekBackoffDynamo(ctx context.Context) (*framework.QueuedPodInfo, error) {
	now := time.Now()
	nowMillis := now.UnixMilli()

	// For custom sorting, we need to fetch and compare multiple items
	if q.cfg.LessFunc != nil && q.cfg.QueueID == "backoffQ" {
		qout, err := q.dynamoClient.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(q.cfg.TableName),
			IndexName:              aws.String(IndexTSName),
			KeyConditionExpression: aws.String("PK = :pk AND TS <= :now"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk":  &types.AttributeValueMemberS{Value: q.pk()},
				":now": &types.AttributeValueMemberN{Value: upperBoundTS(nowMillis)},
			},
			Limit:            aws.Int32(100),
			ScanIndexForward: aws.Bool(true),
			ConsistentRead:   aws.Bool(false),
		})
		if err != nil {
			return nil, err
		}
		if len(qout.Items) == 0 {
			return nil, nil
		}

		// Find the best item according to LessFunc
		var selectedPod *framework.QueuedPodInfo
		for _, item := range qout.Items {
			payloadAttr, ok := item["Payload"].(*types.AttributeValueMemberB)
			if !ok {
				continue
			}

			pInfo, err := UnmarshalQueuedPodInfo(payloadAttr.Value)
			if err != nil {
				klog.Error(err, "Failed to unmarshal pod info", "item", item)
				continue // skip this item if unmarshalling fails
			}

			if selectedPod == nil || q.cfg.LessFunc(pInfo, selectedPod) {
				selectedPod = pInfo
			}
		}

		return selectedPod, nil
	}

	// Simple peek for time-based sorting
	qout, err := q.dynamoClient.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(q.cfg.TableName),
		IndexName:              aws.String(IndexTSName),
		KeyConditionExpression: aws.String("PK = :pk AND TS <= :now"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: q.pk()},
			":now": &types.AttributeValueMemberN{Value: upperBoundTS(nowMillis)},
		},
		Limit:            aws.Int32(1),
		ScanIndexForward: aws.Bool(true),
		ConsistentRead:   aws.Bool(false),
	})
	if err != nil {
		return nil, err
	}
	if len(qout.Items) == 0 {
		return nil, nil
	}

	// Unmarshal and return
	head := qout.Items[0]
	payloadAttr, ok := head["Payload"].(*types.AttributeValueMemberB)
	if !ok {
		return nil, fmt.Errorf("payload attribute missing")
	}

	pInfo, err := UnmarshalQueuedPodInfo(payloadAttr.Value)
	if err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	return pInfo, nil
}

func (q *PriorityQueueAWS) peekActiveDynamo(ctx context.Context) (*framework.QueuedPodInfo, error) {
	if q.dynamoClient == nil {
		return nil, fmt.Errorf("dynamo client not initialised")
	}

	// Query for the head of the queue (smallest TS = highest priority)
	qout, err := q.dynamoClient.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(q.cfg.TableName),
		IndexName:              aws.String(IndexTSName),
		KeyConditionExpression: aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: q.pk()},
		},
		Limit:            aws.Int32(1),
		ScanIndexForward: aws.Bool(true), // ascending: most-negative SK first
		ConsistentRead:   aws.Bool(false),
	})
	if err != nil {
		return nil, err
	}
	if len(qout.Items) == 0 {
		return nil, nil // queue is empty
	}

	// Extract and unmarshal the payload
	head := qout.Items[0]
	payloadAttr, ok := head["Payload"].(*types.AttributeValueMemberB)
	if !ok {
		return nil, fmt.Errorf("payload attribute missing")
	}

	pInfo, err := UnmarshalQueuedPodInfo(payloadAttr.Value)
	if err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	return pInfo, nil
}

// ---------------------------  HELPERS  --------------------------- //

// Prepare the DynamoDB item with proper sort key based on queue type
func (q *PriorityQueueAWS) prepareDynamoItem(pInfo *framework.QueuedPodInfo) (map[string]types.AttributeValue, error) {
	// 1. priority value ------------------------------------------------------
	pr := int32(0)
	if pInfo.Pod.Spec.Priority != nil {
		pr = *pInfo.Pod.Spec.Priority
	}

	// 2. choose time component ----------------------------------------------
	var (
		ts int64
		bo time.Time
	)

	if q.isBackoffQueue() {
		if q.cfg.GetBackoffTime == nil {
			klog.Error(nil, "Backoff time not set", "pod", pInfo.Pod)
			return nil, fmt.Errorf("backoff time not set for pod %s", pInfo.Pod.UID)
		}
		bo = q.cfg.GetBackoffTime(pInfo)
		ts = encodeBackoffSK(bo.UnixMilli(), pr, q.cfg.PriorityAware)
	} else {
		// Ensure we *persist* the same timestamp we use for TS
		if pInfo.Timestamp.IsZero() {
			pInfo.Timestamp = time.Now()
		}
		ts = encodeActiveSK(pr, pInfo.Timestamp)
	}

	// 3. marshal payload -----------------------------------------------------
	payload, err := MarshalQueuedPodInfo(pInfo)
	if err != nil {
		return nil, fmt.Errorf("marshal pod: %w", err)
	}

	// 4. build the item ------------------------------------------------------
	item := map[string]types.AttributeValue{
		"PK":         &types.AttributeValueMemberS{Value: q.pk()},
		"SK":         &types.AttributeValueMemberS{Value: string(pInfo.Pod.UID)},
		AttrTS:       &types.AttributeValueMemberN{Value: strconv.FormatInt(ts, 10)},
		AttrPriority: &types.AttributeValueMemberN{Value: strconv.FormatInt(int64(pr), 10)},
		"Payload":    &types.AttributeValueMemberB{Value: payload},
		"Timestamp":  &types.AttributeValueMemberN{Value: strconv.FormatInt(pInfo.Timestamp.UnixMilli(), 10)},
		"Attempts":   &types.AttributeValueMemberN{Value: strconv.Itoa(pInfo.Attempts)},
	}
	if q.isBackoffQueue() {
		item["BackoffCompletes"] = &types.AttributeValueMemberN{
			Value: strconv.FormatInt(bo.UnixMilli(), 10),
		}
	}
	return item, nil
}

// popBackoff handles simple time-based backoff queues
func (q *PriorityQueueAWS) popBackoff(ctx context.Context) (*framework.QueuedPodInfo, error) {
	for {
		qout, err := q.dynamoClient.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(q.cfg.TableName),
			IndexName:              aws.String(IndexTSName), // *** GSI ***
			KeyConditionExpression: aws.String("PK = :pk AND TS <= :now"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk":  &types.AttributeValueMemberS{Value: q.pk()},
				":now": &types.AttributeValueMemberN{Value: upperBoundTS(time.Now().UnixMilli())},
			},
			Limit:            aws.Int32(1),
			ScanIndexForward: aws.Bool(true),
			ConsistentRead:   aws.Bool(false),
		})
		if err != nil {
			return nil, err
		}
		if len(qout.Items) == 0 {
			return nil, nil
		}
		uid := qout.Items[0]["SK"].(*types.AttributeValueMemberS).Value
		pi, err := q.deleteByPodUID(ctx, uid)
		if err != nil {
			var ccfe *types.ConditionalCheckFailedException
			if errors.As(err, &ccfe) {
				continue
			}
			return nil, err
		}
		return pi, nil
	}
}

// popBackoffWithPriority handles backoff queues with priority-aware sorting
func (q *PriorityQueueAWS) popBackoffWithPriority(ctx context.Context) (*framework.QueuedPodInfo, error) {
	// Fetch a batch of items with completed backoff
	qout, err := q.dynamoClient.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(q.cfg.TableName),
		IndexName:              aws.String(IndexTSName),
		KeyConditionExpression: aws.String("PK = :pk AND TS <= :now"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: q.pk()},
			":now": &types.AttributeValueMemberN{Value: upperBoundTS(time.Now().UnixMilli())},
		},
		Limit:            aws.Int32(100), // Fetch more items for comparison
		ScanIndexForward: aws.Bool(true),
		ConsistentRead:   aws.Bool(false),
	})
	if err != nil {
		return nil, err
	}
	if len(qout.Items) == 0 {
		return nil, nil // No items with completed backoff
	}

	// Find the item that should be popped according to the LessFunc
	var selectedItem map[string]types.AttributeValue
	var selectedPod *framework.QueuedPodInfo

	for _, item := range qout.Items {
		// Unmarshal the pod info
		payloadAttr, ok := item["Payload"].(*types.AttributeValueMemberB)
		if !ok {
			continue
		}

		pInfo, err := UnmarshalQueuedPodInfo(payloadAttr.Value)
		if err != nil {
			klog.Error(err, "Failed to unmarshal pod info", "item", item)
			continue // skip this item if unmarshalling fails
		}

		// Apply the comparison function
		if selectedPod == nil || q.cfg.LessFunc(pInfo, selectedPod) {
			selectedItem = item
			selectedPod = pInfo
		}
	}

	if selectedItem == nil {
		return nil, nil
	}

	// Delete the selected item
	uid := selectedItem["SK"].(*types.AttributeValueMemberS).Value
	for {
		pi, err := q.deleteByPodUID(ctx, uid)
		if err != nil {
			var ccfe *types.ConditionalCheckFailedException
			if errors.As(err, &ccfe) {
				// Someone else got it, retry selection
				return q.popBackoffWithPriority(ctx)
			}
			return nil, err
		}
		return pi, nil
	}
}

// deleteByPodUID deletes a pod from the DynamoDB queue by its UID and returns the deleted pod info.
func (q *PriorityQueueAWS) deleteByPodUID(ctx context.Context, uid string) (*framework.QueuedPodInfo, error) {
	dout, err := q.dynamoClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(q.cfg.TableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: q.pk()},
			"SK": &types.AttributeValueMemberS{Value: uid},
		},
		ConditionExpression: aws.String("attribute_exists(PK)"),
		ReturnValues:        types.ReturnValueAllOld,
	})
	if err != nil {
		return nil, err
	}

	pa, ok := dout.Attributes["Payload"].(*types.AttributeValueMemberB)
	if !ok {
		return nil, fmt.Errorf("payload missing")
	}
	return UnmarshalQueuedPodInfo(pa.Value)
}

func encodeActiveSK(priority int32, t time.Time) int64 {
	// Build a 64-bit key whose natural ascending order is:
	// higher priority first (negated 16-bit),
	// then older millis,
	// with an 8-bit random suffix for uniqueness.

	capped := priority
	switch {
	case capped < 0:
		capped = 0
	case capped > 0xFFFF:
		capped = 0xFFFF
	}

	hi := int64(-capped) << 48
	lo := t.UnixMilli()*milliFactor + int64(rnd.Intn(uniqRange))
	return hi | (lo & ((1 << 48) - 1))
}

func encodeBackoffSK(backoffMillis int64, pr int32, withPr bool) int64 {
	// Build a 64-bit back-off key whose ascending order is:
	// earlier back-off-completion millis first,
	// then (optionally) higher priority via an 8-bit bucket,
	// with a tiny random slice for uniqueness.

	suffix := int64(rnd.Intn(uniqRange)) // 0-9
	if withPr {
		bucket := pr
		if bucket < 0 {
			bucket = 0
		} else if bucket > maxPrBucket {
			bucket = maxPrBucket
		}
		suffix += int64(maxPrBucket-int(bucket)) * uniqRange
	}
	return backoffMillis*milliFactor + suffix
}

func upperBoundTS(nowMillis int64) string {
	return strconv.FormatInt(nowMillis*milliFactor+maxSuffix, 10)
}

func (q *PriorityQueueAWS) isBackoffQueue() bool {
	return q.cfg.QueueID == "backoffQ" || q.cfg.QueueID == "errorBackoffQ"
}

func (q *PriorityQueueAWS) pk() string { return q.cfg.QueueID }

func reverse[T any](s []T) []T {
	r := make([]T, len(s))
	for i, v := range s {
		r[len(s)-1-i] = v
	}
	return r
}

func (q *PriorityQueueAWS) ensureDynamoTable(ctx context.Context, ddb *dynamodb.Client, name string) error {
	// fast-path: table exists
	if _, err := ddb.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(name),
	}); err == nil {
		return nil
	} else if !errors.As(err, new(*types.ResourceNotFoundException)) {
		return err // real failure
	}

	// create with PK/SK schema
	_, err := ddb.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(name),
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("PK"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("SK"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String(AttrTS), AttributeType: types.ScalarAttributeTypeN},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("PK"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("SK"), KeyType: types.KeyTypeRange},
		},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{
			{
				IndexName: aws.String(IndexTSName),
				KeySchema: []types.KeySchemaElement{
					{AttributeName: aws.String("PK"), KeyType: types.KeyTypeHash},
					{AttributeName: aws.String(AttrTS), KeyType: types.KeyTypeRange},
				},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			},
		},
		BillingMode: types.BillingModePayPerRequest,
	})
	return err
}

func (q *PriorityQueueAWS) ensureSQSQueues(ctx context.Context) error {
	nb := q.cfg.NumBuckets
	if nb <= 0 {
		nb = defaultBuckets
	}

	prefix := q.cfg.QueuePrefix
	if prefix == "" {
		prefix = "scheduler-priority"
	}

	ensure := func(name string) (string, error) {
		out, err := q.sqsClient.CreateQueue(ctx, &sqs.CreateQueueInput{
			QueueName: aws.String(name),
		})
		if err != nil {
			return "", err
		}
		return *out.QueueUrl, nil
	}

	// Create the system and critical queues, and the bucket queues.
	var err error
	if q.systemURL, err = ensure(prefix + "-system"); err != nil {
		return err
	}
	if q.criticalURL, err = ensure(prefix + "-critical"); err != nil {
		return err
	}

	q.bucketURLs = make([]string, nb)
	for i := 0; i < nb; i++ {
		if q.bucketURLs[i], err = ensure(
			fmt.Sprintf("%s-bucket-%d", prefix, i)); err != nil {
			return err
		}
	}
	q.bucketSize = normalMaxPriority / int32(nb)
	return nil
}

// ---------------------------  SQS (NOT FINISHED)  --------------------------- //

func (q *PriorityQueueAWS) addSQS(ctx context.Context, p *framework.QueuedPodInfo) error {
	if p == nil || p.Pod == nil {
		return fmt.Errorf("nil pod")
	}

	// pick priority value
	var pr int32
	if p.Pod.Spec.Priority != nil {
		pr = *p.Pod.Spec.Priority
	}

	if pr < 0 {
		pr = 0 // ensure non-negative priority
	}

	// choose queue
	var url string
	switch {
	case pr >= systemPriorityVal:
		url = q.systemURL
	case pr >= criticalPriorityVal:
		url = q.criticalURL
	default:
		idx := pr / q.bucketSize
		if idx >= int32(len(q.bucketURLs)) {
			idx = int32(len(q.bucketURLs)) - 1
		}
		url = q.bucketURLs[idx]
	}

	body, err := json.Marshal(p)
	if err != nil {
		return err
	}

	_, err = q.sqsClient.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(url),
		MessageBody: aws.String(string(body)),
	})
	return err
}

func (q *PriorityQueueAWS) popSQS(ctx context.Context) (*framework.QueuedPodInfo, error) {
	tryQueues := append([]string{q.systemURL, q.criticalURL},
		reverse(q.bucketURLs)...)

	for _, url := range tryQueues {
		out, err := q.sqsClient.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(url),
			MaxNumberOfMessages: 1,
			WaitTimeSeconds:     0, // short poll; caller can loop / back-off
		})
		if err != nil {
			return nil, err
		}
		if len(out.Messages) == 0 {
			continue
		}

		msg := out.Messages[0]
		// delete immediately (at-least-once semantics)
		_, _ = q.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
			QueueUrl:      aws.String(url),
			ReceiptHandle: msg.ReceiptHandle,
		})

		var p framework.QueuedPodInfo
		if err := json.Unmarshal([]byte(*msg.Body), &p); err != nil {
			return nil, err
		}
		return &p, nil
	}
	return nil, nil // all queues empty
}

func (q *PriorityQueueAWS) peekSQS(ctx context.Context) (*framework.QueuedPodInfo, error) {
	// For SQS, we need to check queues in priority order
	tryQueues := append([]string{q.systemURL, q.criticalURL},
		reverse(q.bucketURLs)...)

	for _, url := range tryQueues {
		// Receive message with visibility timeout of 0 to peek without hiding
		out, err := q.sqsClient.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(url),
			MaxNumberOfMessages: 1,
			VisibilityTimeout:   0, // Don't hide the message
			WaitTimeSeconds:     0, // Short poll
		})
		if err != nil {
			return nil, err
		}
		if len(out.Messages) == 0 {
			continue
		}

		// Parse the message body
		msg := out.Messages[0]
		var p framework.QueuedPodInfo
		if err := json.Unmarshal([]byte(*msg.Body), &p); err != nil {
			return nil, err
		}
		return &p, nil
	}
	return nil, nil // all queues empty
}

// ---------------------------  DEPRECATED  --------------------------- //

// Add enqueues a pod into the priority queue.
func (q *PriorityQueueAWS) Add(ctx context.Context, pInfo *framework.QueuedPodInfo) error {
	switch q.cfg.Backend {
	case BackendDynamoDB:
		return q.addDynamo(ctx, pInfo)
	case BackendSQS:
		return q.addSQS(ctx, pInfo)
	default:
		return fmt.Errorf("backend %s not implemented", q.cfg.Backend)
	}
}

func (q *PriorityQueueAWS) addDynamo(ctx context.Context, pInfo *framework.QueuedPodInfo) error {
	if pInfo == nil || pInfo.Pod == nil {
		return fmt.Errorf("nil pod")
	}

	// 1 — priority (handle the *int32 pointer) and sort key
	var pr int32
	if pInfo.Pod.Spec.Priority != nil {
		pr = *pInfo.Pod.Spec.Priority
	}

	var sk int64
	//if q.cfg.QueueID == "backoffQ" || q.cfg.QueueID == "errorBackoffQ" {
	//	sk = getBackoffTime(pInfo).UnixMilli()
	//} else {
	sk = encodeActiveSK(pr, time.Now())
	//}

	// 2 — marshal the payload
	payload, err := json.Marshal(pInfo)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	// 3 — build the PutItem request
	item := map[string]types.AttributeValue{
		"PK":        &types.AttributeValueMemberS{Value: q.pk()},
		"SK":        &types.AttributeValueMemberN{Value: strconv.FormatInt(sk, 10)},
		"Priority":  &types.AttributeValueMemberN{Value: strconv.FormatInt(int64(pr), 10)},
		"Payload":   &types.AttributeValueMemberB{Value: payload},
		"PodUID":    &types.AttributeValueMemberS{Value: string(pInfo.Pod.UID)},
		"Timestamp": &types.AttributeValueMemberN{Value: strconv.FormatInt(pInfo.Timestamp.UnixMilli(), 10)},
		"Attempts":  &types.AttributeValueMemberN{Value: strconv.Itoa(pInfo.Attempts)},
	}

	_, err = q.dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(q.cfg.TableName),
		Item:      item,
	})
	return err
}
