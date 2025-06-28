package awsqueues_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	awsqueues "k8s.io/kubernetes/pkg/scheduler/awsqueues"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

////////////////////////////////////////////////////////////////////////////////
// Active Queue (DynamoDB)
////////////////////////////////////////////////////////////////////////////////

func TestDynamoDefaultPriority(t *testing.T) {
	ctx, pq := setUpFreshActiveQueue(t)

	hi := dummyPod(10)
	low := dummyPodPtrNil()

	if err := pq.AddOrUpdate(ctx, low); err != nil {
		t.Fatalf("add low: %v", err)
	}
	if err := pq.AddOrUpdate(ctx, hi); err != nil {
		t.Fatalf("add hi: %v", err)
	}

	first, _ := pq.Pop(ctx)
	second, _ := pq.Pop(ctx)

	if pri(first) != 10 || pri(second) != 0 {
		t.Fatalf("expected pop order [10,0] got [%d,%d]",
			pri(first), pri(second))
	}
}

func TestDynamoPriorityOrder(t *testing.T) {
	ctx, pq := setUpFreshActiveQueue(t)

	for _, p := range []int32{10, 30, 20} {
		if err := pq.AddOrUpdate(ctx, dummyPod(p)); err != nil {
			t.Fatalf("Add pr=%d: %v", p, err)
		}
	}

	want := []int32{30, 20, 10}
	var got []int32
	for range want {
		q, _ := pq.Pop(ctx)
		got = append(got, pri(q))
	}
	if !slices.Equal(got, want) {
		t.Errorf("priority order wrong: got %v, want %v", got, want)
	}
}

func TestDynamoFIFOWithinPriority(t *testing.T) {
	ctx, pq := setUpFreshActiveQueue(t)

	first := dummyPod(55)
	second := dummyPod(55)

	if err := pq.AddOrUpdate(ctx, first); err != nil {
		t.Fatalf("add first: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := pq.AddOrUpdate(ctx, second); err != nil {
		t.Fatalf("add second: %v", err)
	}

	pop1, _ := pq.Pop(ctx)
	pop2, _ := pq.Pop(ctx)

	if pop1.Pod.UID != first.Pod.UID || pop2.Pod.UID != second.Pod.UID {
		t.Errorf("FIFO violated within same priority: popped %s then %s",
			pop1.Pod.Name, pop2.Pod.Name)
	}
}

func TestDelete(t *testing.T) {
	ctx, pq := setUpFreshActiveQueue(t)

	// Add pods
	pod1 := createPodWithNameAndPriority("pod-1", 50)
	pod2 := createPodWithNameAndPriority("pod-2", 100)
	pod3 := createPodWithNameAndPriority("pod-3", 75)

	err := pq.AddOrUpdate(ctx, pod1)
	if err != nil {
		return
	}
	err = pq.AddOrUpdate(ctx, pod2)
	if err != nil {
		return
	}
	err = pq.AddOrUpdate(ctx, pod3)
	if err != nil {
		return
	}

	// Verify count
	count, _ := pq.Count(ctx)
	if count != 3 {
		t.Errorf("Expected count 3, got %d", count)
	}

	// Delete pod2 (highest priority)
	err = pq.Delete(ctx, pod2)
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify count decreased
	count, _ = pq.Count(ctx)
	if count != 2 {
		t.Errorf("Expected count 2 after delete, got %d", count)
	}

	// Pop should return pod3 (next highest priority)
	item, _ := pq.Pop(ctx)
	if item.Pod.Name != "pod-3" {
		t.Errorf("Expected pod-3, got %s", item.Pod.Name)
	}

	// Delete non-existent pod should not error
	fakePod := createPodWithNameAndPriority("fake", 200)
	err = pq.Delete(ctx, fakePod)
	if err != nil {
		t.Errorf("Delete non-existent pod should not error, got: %v", err)
	}
}

func TestList(t *testing.T) {
	ctx, pq := setUpFreshActiveQueue(t)

	// Add pods with different priorities
	pods := []*framework.QueuedPodInfo{
		createPodWithNameAndPriority("pod-1", 10),
		createPodWithNameAndPriority("pod-2", 50),
		createPodWithNameAndPriority("pod-3", 30),
	}

	for _, pod := range pods {
		err := pq.AddOrUpdate(ctx, pod)
		if err != nil {
			return
		}
	}

	// List all pods
	list, err := pq.List(ctx)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(list) != 3 {
		t.Errorf("Expected 3 pods in list, got %d", len(list))
	}

	// Verify all pods are present
	names := make(map[string]bool)
	for _, item := range list {
		names[item.Pod.Name] = true
	}

	for _, pod := range pods {
		if !names[pod.Pod.Name] {
			t.Errorf("Pod %s missing from list", pod.Pod.Name)
		}
	}
}

func TestNilPodHandling(t *testing.T) {
	ctx, pq := setUpFreshActiveQueue(t)

	// Test nil pod
	err := pq.AddOrUpdate(ctx, nil)
	if err == nil {
		t.Error("Expected error for nil pod")
	}

	// Test pod with nil PodInfo
	err = pq.AddOrUpdate(ctx, &framework.QueuedPodInfo{})
	if err == nil {
		t.Error("Expected error for nil pod.Pod")
	}

	// Test pod with nil priority (should default to 0)
	pod := &framework.QueuedPodInfo{
		PodInfo: &framework.PodInfo{
			Pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "nil-priority",
					UID:  "nil-priority-uid",
				},
				Spec: v1.PodSpec{}, // Priority is nil
			},
		},
		Timestamp: time.Now(),
	}

	err = pq.AddOrUpdate(ctx, pod)
	if err != nil {
		t.Errorf("Should handle nil priority, got error: %v", err)
	}

	// Add a pod with priority 1
	pod2 := createPodWithPriority(1)
	err = pq.AddOrUpdate(ctx, pod2)
	if err != nil {
		return
	}

	// Pop should return priority 1 first
	item, _ := pq.Pop(ctx)
	if *item.Pod.Spec.Priority != 1 {
		t.Error("Expected priority 1 pod first")
	}

	// Next pop should return nil priority pod
	item, _ = pq.Pop(ctx)
	if item.Pod.Name != "nil-priority" {
		t.Error("Expected nil-priority pod")
	}
}

func TestDynamoPeekDoesNotRemove(t *testing.T) {
	ctx, pq := setUpFreshActiveQueue(t)

	pTop := createPodWithNameAndPriority("top", 99)
	pLower := createPodWithNameAndPriority("low", 1)

	if err := pq.AddOrUpdate(ctx, pLower); err != nil {
		t.Fatal(err)
	}
	if err := pq.AddOrUpdate(ctx, pTop); err != nil {
		t.Fatal(err)
	}

	peek, _ := pq.Peek(ctx)
	if peek.Pod.Name != "top" {
		t.Fatalf("peek returned %s, want top", peek.Pod.Name)
	}

	// After peek, the queue length must be unchanged
	if cnt, _ := pq.Count(ctx); cnt != 2 {
		t.Fatalf("peek removed item, count=%d", cnt)
	}
}

func TestDynamoUpdateKeepsSingleEntry(t *testing.T) {
	ctx, pq := setUpFreshActiveQueue(t)

	orig := createPodWithNameAndPriority("victim", 10)
	if err := pq.AddOrUpdate(ctx, orig); err != nil {
		t.Fatal(err)
	}

	// same UID, higher priority
	update := createPodWithNameAndPriority("victim-new", 123)
	update.Pod.UID = orig.Pod.UID
	if err := pq.AddOrUpdate(ctx, update); err != nil {
		t.Fatal(err)
	}

	if cnt, _ := pq.Count(ctx); cnt != 1 {
		t.Fatalf("update caused dup, count=%d", cnt)
	}
	picked, _ := pq.Pop(ctx)
	if *picked.Pod.Spec.Priority != 123 {
		t.Fatalf("priority not updated, got %d", pri(picked))
	}
}

func TestDynamoPopEmpty(t *testing.T) {
	ctx, pq := setUpFreshActiveQueue(t)

	got, err := pq.Pop(ctx)
	if err != nil || got != nil {
		t.Fatalf("expected (nil,nil) on empty queue, got %#v / %v", got, err)
	}
}

func TestDynamoCountLifecycle(t *testing.T) {
	ctx, pq := setUpFreshActiveQueue(t)

	for i := 0; i < 5; i++ {
		if err := pq.AddOrUpdate(ctx, dummyPod(int32(i))); err != nil {
			t.Fatal(err)
		}
	}
	if cnt, _ := pq.Count(ctx); cnt != 5 {
		t.Fatalf("want 5, got %d", cnt)
	}

	// delete two
	for i := 0; i < 2; i++ {
		p, _ := pq.Pop(ctx) // pop returns highest pr each time
		if err := pq.Delete(ctx, p); err != nil {
			t.Fatal(err)
		} // deleting a just-popped item is a nop
	}
	if cnt, _ := pq.Count(ctx); cnt != 3 {
		t.Fatalf("after delete want 3, got %d", cnt)
	}

	// drain the rest
	for i := 0; i < 3; i++ {
		_, _ = pq.Pop(ctx)
	}
	if cnt, _ := pq.Count(ctx); cnt != 0 {
		t.Fatalf("queue should be empty, count=%d", cnt)
	}
}

func TestDynamoDuplicateUIDIgnored(t *testing.T) {
	ctx, pq := setUpFreshActiveQueue(t)

	p := createPodWithNameAndPriority("dup", 7)
	if err := pq.AddOrUpdate(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := pq.AddOrUpdate(ctx, p); err != nil {
		t.Fatal(err)
	} // same object again

	if cnt, _ := pq.Count(ctx); cnt != 1 {
		t.Fatalf("duplicate UID created extra row, count=%d", cnt)
	}
}

func TestDynamoUpdateToLowerPriority(t *testing.T) {
	ctx, pq := setUpFreshActiveQueue(t)

	hi := createPodWithNameAndPriority("stay-high", 90)
	lo := createPodWithNameAndPriority("will-drop", 80)

	must(t, pq.AddOrUpdate(ctx, hi))
	must(t, pq.AddOrUpdate(ctx, lo))

	// downgrade the second Pod
	loNew := createPodWithNameAndPriority("will-drop-new", 10)
	loNew.Pod.UID = lo.Pod.UID
	must(t, pq.AddOrUpdate(ctx, loNew))

	if cnt, _ := pq.Count(ctx); cnt != 2 {
		t.Fatalf("want 2 rows after downgrade, got %d", cnt)
	}

	p1, _ := pq.Pop(ctx)
	p2, _ := pq.Pop(ctx)

	if p1.Pod.Name != "stay-high" || p2.Pod.Name != "will-drop-new" {
		t.Fatalf("wrong order after downgrade: popped %s then %s",
			p1.Pod.Name, p2.Pod.Name)
	}
}

func TestDynamoPeekAfterUpdate(t *testing.T) {
	ctx, pq := setUpFreshActiveQueue(t)

	p := createPodWithNameAndPriority("victim", 40)
	must(t, pq.AddOrUpdate(ctx, p))

	// peek #1
	first, _ := pq.Peek(ctx)
	if pri(first) != 40 {
		t.Fatalf("peek 1 priority=%d, want 40", pri(first))
	}

	// raise its priority
	pNew := createPodWithNameAndPriority("victim-v2", 99)
	pNew.Pod.UID = p.Pod.UID
	must(t, pq.AddOrUpdate(ctx, pNew))

	// peek #2 should see new priority
	second, _ := pq.Peek(ctx)
	if pri(second) != 99 {
		t.Fatalf("peek 2 priority=%d, want 99", pri(second))
	}
}

func TestDynamoLongFIFOStability(t *testing.T) {
	ctx, pq := setUpFreshActiveQueue(t)

	const n = 20
	order := make([]string, 0, n)

	for i := 0; i < n; i++ {
		p := createPodWithNameAndPriority(fmt.Sprintf("pod-%02d", i), 17)
		order = append(order, p.Pod.Name)
		must(t, pq.AddOrUpdate(ctx, p))
		time.Sleep(200 * time.Microsecond) // distinct TS
	}

	for i := 0; i < n; i++ {
		item, _ := pq.Pop(ctx)
		if item.Pod.Name != order[i] {
			t.Fatalf("FIFO broken at pos %d: got %s, want %s",
				i, item.Pod.Name, order[i])
		}
	}
}

func TestDynamoConcurrentAddPop(t *testing.T) {
	ctx, pq := setUpFreshActiveQueue(t)

	const workers = 8
	const perWorker = 25
	wg := sync.WaitGroup{}

	// producers
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				name := fmt.Sprintf("w%d-%d", id, i)
				p := createPodWithNameAndPriority(name, int32(i))
				_ = pq.AddOrUpdate(ctx, p)
			}
		}(w)
	}

	// consumer
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			item, _ := pq.Pop(ctx)
			if item == nil {
				// retry a few times – producers may still be writing
				time.Sleep(10 * time.Millisecond)
				if cnt, _ := pq.Count(ctx); cnt == 0 {
					return
				}
				continue
			}
			// delete is a no-op but stresses the Delete path
			_ = pq.Delete(ctx, item)
		}
	}()

	wg.Wait()

	if cnt, _ := pq.Count(ctx); cnt != 0 {
		t.Fatalf("queue leak: %d items left", cnt)
	}
}

func TestDynamoListSnapshotIsolation(t *testing.T) {
	ctx, pq := setUpFreshActiveQueue(t)

	for _, pr := range []int32{5, 15, 25} {
		must(t, pq.AddOrUpdate(ctx, dummyPod(pr)))
	}

	list, _ := pq.List(ctx)
	// corrupt the snapshot deliberately
	for _, q := range list {
		if q.Pod.Spec.Priority != nil {
			*q.Pod.Spec.Priority = 999
		}
	}

	// Pop should still return the real highest priority (25)
	item, _ := pq.Pop(ctx)
	if pri(item) != 25 {
		t.Fatalf("snapshot corruption affected queue, got %d", pri(item))
	}
}

func TestDynamoCountAccuracyUnderChurn(t *testing.T) {
	ctx, pq := setUpFreshActiveQueue(t)

	for i := 0; i < 50; i++ {
		p := dummyPod(int32(i))
		must(t, pq.AddOrUpdate(ctx, p))
		if i%3 == 0 { // every third iteration pop & delete
			q, _ := pq.Pop(ctx)
			_ = pq.Delete(ctx, q)
		}
	}

	// we deleted at i = 0,3,6,...,48 → 17 deletions in total
	want := 50 - ((50 + 2) / 3) // = 50 - 17 = 33
	if cnt, _ := pq.Count(ctx); cnt != want {
		t.Fatalf("count=%d, want %d", cnt, want)
	}
}

func TestPopEmptyReturnsError(t *testing.T) {
	ctx, pq := setUpFreshActiveQueue(t)
	got, err := pq.Pop(ctx)
	if err == nil || got != nil {
		t.Fatalf("expected (nil, error), got (%v, %v)", got, err)
	}
}

////////////////////////////////////////////////////////////////////////////////
// Helpers
////////////////////////////////////////////////////////////////////////////////

func setUpFreshActiveQueue(t *testing.T) (context.Context, *awsqueues.PriorityQueueAWS) {
	t.Helper()
	ctx := context.Background()

	awsCfg := mustCfg(t)
	localOpt := func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(dynaLocalURL) }
	ddb := dynamodb.NewFromConfig(awsCfg, localOpt)

	// Drop table so constructor must recreate it
	tableName := "test_activeq"
	_, _ = ddb.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(tableName)})

	pq, err := awsqueues.NewPriorityQueueAWS(ctx, awsCfg, awsqueues.Config{
		Backend:   awsqueues.BackendDynamoDB,
		TableName: tableName,
		QueueID:   "activeQ",
	}, []func(*dynamodb.Options){localOpt}, nil)
	if err != nil {
		t.Fatalf("Failed to create active queue: %v", err)
	}
	return ctx, pq
}
