package awstest

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"k8s.io/kubernetes/pkg/scheduler/awsstore"
	"slices"
	"testing"
	"time"
)

////////////////////////////////////////////////////////////////////////////////
// SQS
////////////////////////////////////////////////////////////////////////////////

// TestSQSGlobalOrdering verifies that SQS respects global priority ordering
func TestSQSGlobalOrdering(t *testing.T) {
	ctx, pq := setUpFreshSQS(t, 5) // 5 buckets + critical + system

	// priorities mapped to different queues
	cases := []int32{50, 999_999_999, systemPriorityVal, criticalPriorityVal, 400_000_000}
	want := []int32{systemPriorityVal, criticalPriorityVal, 999_999_999, 400_000_000, 50}

	for _, p := range cases {
		_ = pq.Add(ctx, dummyPod(p))
	}

	var got []int32
	for range cases {
		if itm, _ := pq.Pop(ctx); itm != nil {
			got = append(got, *itm.Pod.Spec.Priority)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("SQS global order wrong: got %v want %v", got, want)
	}
}

// TestSQSFIFOWithinBucket verifies that within a single bucket, Pods are popped
func TestSQSFIFOWithinBucket(t *testing.T) {
	ctx, pq := setUpFreshSQS(t, 5)

	pr := int32(123) // lands in bucket-0
	a := dummyPod(pr)
	time.Sleep(2 * time.Millisecond)
	b := dummyPod(pr)

	_ = pq.Add(ctx, a)
	_ = pq.Add(ctx, b)

	pop1, _ := pq.Pop(ctx)
	pop2, _ := pq.Pop(ctx)

	if pop1.Pod.UID != a.Pod.UID || pop2.Pod.UID != b.Pod.UID {
		t.Fatalf("FIFO violated within bucket: pop1=%s pop2=%s", pop1.Pod.UID, pop2.Pod.UID)
	}
}

////////////////////////////////////////////////////////////////////////////////
// Helpers
////////////////////////////////////////////////////////////////////////////////

func setUpFreshSQS(t *testing.T, buckets int) (context.Context, *awsstore.PriorityQueueAWS) {
	t.Helper()
	ctx := context.Background()

	awsCfg := mustCfg(t)
	localOpt := func(o *sqs.Options) { o.BaseEndpoint = aws.String(sqsLocalURL) }

	// purge any leftover queues
	sqsCli := sqs.NewFromConfig(awsCfg, localOpt)
	qs, _ := sqsCli.ListQueues(ctx, &sqs.ListQueuesInput{QueueNamePrefix: aws.String(queuePrefix)})
	for _, url := range qs.QueueUrls {
		_, _ = sqsCli.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: aws.String(url)})
	}

	pq, err := awsstore.NewPriorityQueueAWS(ctx, awsCfg, awsstore.Config{
		Backend:     awsstore.BackendSQS,
		QueuePrefix: queuePrefix,
		NumBuckets:  buckets,
	}, nil, []func(*sqs.Options){localOpt})
	if err != nil {
		t.Fatalf("queue ctor: %v", err)
	}
	return ctx, pq
}
