package awstest

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

////////////////////////////////////////////////////////////////////////////////
// Constants
////////////////////////////////////////////////////////////////////////////////

const (
	normalMaxPriority   = int32(1_000_000_000)
	criticalPriorityVal = int32(2_000_000_000)
	systemPriorityVal   = int32(2_000_001_000)
	defaultBuckets      = 5
)

const (
	dynaLocalURL = "http://localhost:8000"
	sqsLocalURL  = "http://localhost:4566"

	tableName   = "pq_test"
	region      = "us-east-1"
	queuePrefix = "pqtest"
)

////////////////////////////////////////////////////////////////////////////////
// Helpers
////////////////////////////////////////////////////////////////////////////////

func createPodWithPriority(priority int32) *framework.QueuedPodInfo {
	return createPodWithNameAndPriority(fmt.Sprintf("pod-%d", priority), priority)
}

func createPodWithNameAndPriority(name string, priority int32) *framework.QueuedPodInfo {
	return &framework.QueuedPodInfo{
		PodInfo: &framework.PodInfo{
			Pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: "default",
					UID:       types.UID(fmt.Sprintf("uid-%s-%d", name, time.Now().UnixNano())),
				},
				Spec: v1.PodSpec{
					Priority: pointer.Int32(priority),
				},
			},
		},
		Timestamp: time.Now(),
		Attempts:  0,
	}
}

func dummyPod(pr int32) *framework.QueuedPodInfo {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("pod-%d-%s", pr, rand.String(4)),
			UID:  ktypes.UID(rand.String(20)),
		},
		Spec: v1.PodSpec{Priority: pointer.Int32(pr)},
	}
	return &framework.QueuedPodInfo{
		PodInfo:   &framework.PodInfo{Pod: pod},
		Timestamp: time.Now(),
	}
}

func dummyPodPtrNil() *framework.QueuedPodInfo {
	p := dummyPod(0)
	p.Pod.Spec.Priority = nil
	return p
}

func pri(q *framework.QueuedPodInfo) int32 {
	if q == nil || q.Pod == nil || q.Pod.Spec.Priority == nil {
		return 0
	}
	return *q.Pod.Spec.Priority
}

func mustCfg(t *testing.T) aws.Config {
	t.Helper()
	ctx := context.Background()

	// Fast-fail if local emulator is not up
	if err := ping(ctx, dynaLocalURL); err != nil {
		t.Skip("DynamoDB-Local not running:", err)
	}

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("dummy", "dummy", ""),
		),
	)
	if err != nil {
		t.Fatalf("aws cfg: %v", err)
	}
	return cfg
}

func ping(ctx context.Context, url string) error {
	cfg := aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider("dummy", "dummy", ""),
	}
	cli := dynamodb.New(dynamodb.Options{
		Region:       cfg.Region,
		Credentials:  cfg.Credentials,
		BaseEndpoint: aws.String(url),
	})
	_, err := cli.ListTables(ctx, &dynamodb.ListTablesInput{Limit: aws.Int32(1)})
	return err
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
