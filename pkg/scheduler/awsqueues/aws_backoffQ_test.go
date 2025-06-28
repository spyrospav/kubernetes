package awsqueues_test

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/kubernetes/pkg/scheduler/awsqueues"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"testing"
	"time"
)

////////////////////////////////////////////////////////////////////////////////
// Backoff Queue (DynamoDB)
////////////////////////////////////////////////////////////////////////////////

func TestBackoffTimeOrderOnly(t *testing.T) {
	now := time.Now()
	mc := mockBackoffCalc{m: map[string]time.Time{}}

	less := awsqueues.LessFunc(nil) // nil → compare only by TS
	ctx, pq := setUpBackoffQueue(t, "errorBackoffQ", false, mc.Get, less)

	// three pods finishing back-off in increasing order
	p1 := createPodWithNameAndPriority("p1", 5)
	p2 := createPodWithNameAndPriority("p2", 9)
	p3 := createPodWithNameAndPriority("p3", 42)

	// ready at t-2s, t-1s, t-0.5s
	mc.m[string(p1.Pod.UID)] = now.Add(-2 * time.Second)
	mc.m[string(p2.Pod.UID)] = now.Add(-1 * time.Second)
	mc.m[string(p3.Pod.UID)] = now.Add(-500 * time.Millisecond)

	for _, p := range []*framework.QueuedPodInfo{p1, p2, p3} {
		must(t, pq.AddOrUpdate(ctx, p))
	}

	// Pops must respect back-off finish time, **ignoring priority**.
	want := []string{"p1", "p2", "p3"}
	for _, w := range want {
		got, _ := pq.Pop(ctx)
		if got == nil || got.Pod.Name != w {
			t.Fatalf("pop=%v, want %s", got.Pod.Name, w)
		}
	}
}

func TestBackoffItemNotReadyReturnsNil(t *testing.T) {
	now := time.Now()
	mc := mockBackoffCalc{m: map[string]time.Time{}}

	ctx, pq := setUpBackoffQueue(t, "errorBackoffQ", false, mc.Get, nil)

	p := createPodWithNameAndPriority("future", 1)
	mc.m[string(p.Pod.UID)] = now.Add(5 * time.Second) // 5 s in the future
	must(t, pq.AddOrUpdate(ctx, p))

	if got, _ := pq.Pop(ctx); got != nil {
		t.Fatalf("expected nil pop (not ready), got %s", got.Pod.Name)
	}
}

func TestBackoffPriorityAwareOrdering(t *testing.T) {
	now := time.Now()
	mc := mockBackoffCalc{m: map[string]time.Time{}}

	ctx, pq := setUpBackoffQueue(t, "backoffQ", true, mc.Get,
		lessWithPriority(mc.Get))

	// three pods ready in the SAME millisecond
	pLow := createPodWithNameAndPriority("low", 10)
	pMid := createPodWithNameAndPriority("mid", 50)
	pHigh := createPodWithNameAndPriority("high", 99)

	for _, p := range []*framework.QueuedPodInfo{pLow, pMid, pHigh} {
		mc.m[string(p.Pod.UID)] = now // identical TS
		must(t, pq.AddOrUpdate(ctx, p))
	}

	// pops must follow priority 99 → 50 → 10
	want := []string{"high", "mid", "low"}
	for _, w := range want {
		got, _ := pq.Pop(ctx)
		if got == nil || got.Pod.Name != w {
			t.Fatalf("pop=%v, want %s", got.Pod.Name, w)
		}
	}
}

func TestBackoffUpdateSameUID(t *testing.T) {
	now := time.Now()
	mc := mockBackoffCalc{m: map[string]time.Time{}}

	ctx, pq := setUpBackoffQueue(t, "errorBackoffQ", false, mc.Get, nil)

	orig := createPodWithNameAndPriority("victim", 1)
	mc.m[string(orig.Pod.UID)] = now.Add(5 * time.Second) // not ready yet
	must(t, pq.AddOrUpdate(ctx, orig))

	// rewrite same UID with *earlier* back-off and higher priority
	upd := createPodWithNameAndPriority("victim-new", 77)
	upd.Pod.UID = orig.Pod.UID
	mc.m[string(upd.Pod.UID)] = now.Add(-1 * time.Second) // ready now
	must(t, pq.AddOrUpdate(ctx, upd))

	if cnt, _ := pq.Count(ctx); cnt != 1 {
		t.Fatalf("duplicate row after update, count=%d", cnt)
	}
	got, _ := pq.Pop(ctx)
	if got == nil || *got.Pod.Spec.Priority != 77 {
		t.Fatalf("update ignored, got %+v", got)
	}
}

func TestBackoffPeekDoesNotRemove(t *testing.T) {
	now := time.Now()
	mc := mockBackoffCalc{m: map[string]time.Time{}}
	ctx, pq := setUpBackoffQueue(t, "errorBackoffQ", false, mc.Get, nil)

	ready := createPodWithNameAndPriority("ready", 7)
	mc.m[string(ready.Pod.UID)] = now.Add(-time.Second)
	must(t, pq.AddOrUpdate(ctx, ready))

	peek, _ := pq.Peek(ctx)
	if peek.Pod.Name != "ready" {
		t.Fatalf("peek got %s want ready", peek.Pod.Name)
	}
	if cnt, _ := pq.Count(ctx); cnt != 1 {
		t.Fatalf("peek removed the item, count=%d", cnt)
	}

	pop, _ := pq.Pop(ctx)
	if pop.Pod.Name != "ready" {
		t.Fatalf("pop after peek returned %s", pop.Pod.Name)
	}
}

func TestBackoffCountAfterMixedPops(t *testing.T) {
	base := time.Now()
	mc := mockBackoffCalc{m: map[string]time.Time{}}
	ctx, pq := setUpBackoffQueue(t, "errorBackoffQ", false, mc.Get, nil)

	// 5 ready + 3 future
	for i := 0; i < 8; i++ {
		p := createPodWithNameAndPriority(fmt.Sprintf("p-%d", i), int32(i))
		if i < 5 {
			mc.m[string(p.Pod.UID)] = base.Add(-time.Second)
		} else {
			mc.m[string(p.Pod.UID)] = base.Add(time.Minute)
		}
		must(t, pq.AddOrUpdate(ctx, p))
	}

	// pop all ready ones
	for i := 0; i < 5; i++ {
		if pop, _ := pq.Pop(ctx); pop == nil {
			t.Fatalf("expected ready item #%d", i)
		}
	}
	if pop, _ := pq.Pop(ctx); pop != nil { // sixth Pop must be nil
		t.Fatalf("expected nil after ready batch, got %s", pop.Pod.Name)
	}
	if cnt, _ := pq.Count(ctx); cnt != 3 {
		t.Fatalf("want 3 future items left, count=%d", cnt)
	}
}

func TestBackoffDeleteByUID(t *testing.T) {
	base := time.Now()
	mc := mockBackoffCalc{m: map[string]time.Time{}}
	ctx, pq := setUpBackoffQueue(t, "errorBackoffQ", false, mc.Get, nil)

	keep := createPodWithNameAndPriority("keep", 1)
	drop := createPodWithNameAndPriority("drop", 99)

	mc.m[string(keep.Pod.UID)] = base.Add(-time.Second) // ready
	mc.m[string(drop.Pod.UID)] = base.Add(time.Minute)  // future

	must(t, pq.AddOrUpdate(ctx, keep))
	must(t, pq.AddOrUpdate(ctx, drop))

	must(t, pq.Delete(ctx, drop))

	if cnt, _ := pq.Count(ctx); cnt != 1 {
		t.Fatalf("delete should leave 1, count=%d", cnt)
	}
	if pop, _ := pq.Pop(ctx); pop.Pod.Name != "keep" {
		t.Fatalf("pop expected keep, got %s", pop.Pod.Name)
	}
	if pop, _ := pq.Pop(ctx); pop != nil {
		t.Fatalf("queue should be empty, got %s", pop.Pod.Name)
	}
}

func TestBackoffMassChurnAccuracy(t *testing.T) {
	const n = 1_000
	rand.Seed(time.Now().UnixNano())

	base := time.Now()
	mc := mockBackoffCalc{m: map[string]time.Time{}}
	ctx, pq := setUpBackoffQueue(t, "backoffQ", true, mc.Get, lessWithPriority(mc.Get))

	live := map[string]struct{}{} // ground-truth set of UIDs in queue

	for i := 0; i < n; i++ {
		switch rand.Intn(4) {
		case 0, 1: // 50 % add/update
			p := createPodWithNameAndPriority(fmt.Sprintf("p-%d", rand.Int()), int32(rand.Intn(100)))
			uid := string(p.Pod.UID)
			ready := rand.Intn(2) == 0
			if ready {
				mc.m[uid] = base.Add(-time.Duration(rand.Intn(5)) * time.Second)
			} else {
				mc.m[uid] = base.Add(time.Duration(rand.Intn(30)) * time.Second)
			}
			must(t, pq.AddOrUpdate(ctx, p))
			live[uid] = struct{}{}

		case 2: // 25 % pop (only if ready)
			if pop, _ := pq.Pop(ctx); pop != nil {
				delete(live, string(pop.Pod.UID))
			}

		case 3: // 25 % random delete
			for uid := range live {
				fake := createPodWithNameAndPriority("x", 0)
				fake.Pod.UID = ktypes.UID(uid)
				_ = pq.Delete(ctx, fake)
				delete(live, uid)
				break
			}
		}
	}

	if cnt, _ := pq.Count(ctx); cnt != len(live) {
		t.Fatalf("count=%d, ground=%d", cnt, len(live))
	}
}

////////////////////////////////////////////////////////////////////////////////
// Helpers
////////////////////////////////////////////////////////////////////////////////

func setUpBackoffQueue(
	t *testing.T,
	queueID string,
	priorityAware bool, // true → include priority in ordering
	calcFunc awsqueues.BackoffTimeFunc, // normally mockBackoffCalc.Get
	lessFunc awsqueues.LessFunc, // nil for plain time order
) (context.Context, *awsqueues.PriorityQueueAWS) {
	t.Helper()
	ctx := context.Background()

	awsCfg := mustCfg(t)
	localOpt := func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(dynaLocalURL) }
	ddb := dynamodb.NewFromConfig(awsCfg, localOpt)

	table := "test_" + queueID
	_, _ = ddb.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(table)})

	pq, err := awsqueues.NewPriorityQueueAWS(ctx, awsCfg, awsqueues.Config{
		Backend:        awsqueues.BackendDynamoDB,
		TableName:      table,
		QueueID:        queueID,
		PriorityAware:  priorityAware,
		GetBackoffTime: calcFunc,
		LessFunc:       lessFunc,
	}, []func(*dynamodb.Options){localOpt}, nil)
	if err != nil {
		t.Fatalf("queue create: %v", err)
	}
	return ctx, pq
}

// mockBackoffCalc lets tests dictate each pod’s back-off‐completion time.
type mockBackoffCalc struct{ m map[string]time.Time }

func (mc mockBackoffCalc) Get(p *framework.QueuedPodInfo) time.Time {
	return mc.m[string(p.Pod.UID)]
}

func lessWithPriority(getTS awsqueues.BackoffTimeFunc) awsqueues.LessFunc {
	return func(p1, p2 *framework.QueuedPodInfo) bool {
		t1, t2 := getTS(p1), getTS(p2)
		if !t1.Equal(t2) {
			return t1.Before(t2)
		}
		pr1, pr2 := int32(0), int32(0)
		if p1.Pod.Spec.Priority != nil {
			pr1 = *p1.Pod.Spec.Priority
		}
		if p2.Pod.Spec.Priority != nil {
			pr2 = *p2.Pod.Spec.Priority
		}
		return pr1 > pr2
	}
}
