package awsstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// wireQueuedPod is the “on-the-wire” shape we store in DynamoDB.
type wireQueuedPod struct {
	PodJSON  []byte `json:"pod"`
	Attempts int    `json:"attempts"`
	TS       int64  `json:"ts"`
	Backoff  int64  `json:"backoff,omitempty"`

	// Dropped fields that are optional features in the QueuedPodInfo.
}

type wirePodState struct {
	PodJSON []byte `json:"pod"`
	// Nil deadline encoded as 0
	DeadlineNsec int64 `json:"deadline"`
	Assumed      bool  `json:"assumed"`
	BindingDone  bool  `json:"bindingDone"`
}

type wireNom struct {
	Pod PodRef `json:"pr"`
}

type wireNodeInfo struct {
	NodeJSON []byte   `json:"node,omitempty"`
	PodsJSON [][]byte `json:"pods,omitempty"`
	Gen      int64    `json:"gen,omitempty"`
}

type PodState struct {
	Pod             *v1.Pod
	Deadline        *time.Time
	BindingFinished bool
	Version         int64
}

type PodRef struct {
	name      string
	namespace string
	uid       types.UID
}

type MemNominated struct {
	// nodeName → []podRef
	byNode map[string][]PodRef
	// uid → nodeName
	byUID map[types.UID]string
}

func PodToRef(pod *v1.Pod) PodRef {
	return PodRef{
		name:      pod.Name,
		namespace: pod.Namespace,
		uid:       pod.UID,
	}
}

func (np PodRef) ToPod() *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      np.name,
			Namespace: np.namespace,
			UID:       np.uid,
		},
	}
}

func BuildAWSConfig() aws.Config {
	ctx := context.Background()

	// First, try to load from mounted credentials in /root/.aws
	if _, err := os.Stat("/root/.aws/credentials"); err == nil {
		klog.Infof("Loading AWS credentials from /root/.aws")
		cfg, err := config.LoadDefaultConfig(ctx,
			config.WithRegion("us-east-1"),
			config.WithSharedConfigFiles([]string{"/root/.aws/config"}),
			config.WithSharedCredentialsFiles([]string{"/root/.aws/credentials"}),
			config.WithSharedConfigProfile("kevin_dong"),
		)
		if err != nil {
			klog.Errorf("Failed to load AWS config from mounted credentials: %v", err)
			panic(fmt.Sprintf("buildAWSConfig from mounted creds: %v", err))
		}

		// Log the configuration for debugging
		creds, err := cfg.Credentials.Retrieve(ctx)
		if err != nil {
			klog.Errorf("Failed to retrieve credentials: %v", err)
			panic(fmt.Sprintf("Failed to retrieve credentials: %v", err))
		}
		klog.Infof("AWS credentials loaded successfull. AccessKeyID: %s...", creds.AccessKeyID[:10])

		return cfg
	}

	// Fallback to environment variables or default credential chain
	klog.Infof("Loading AWS credentials from environment/default chain")
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
	)
	if err != nil {
		panic(fmt.Sprintf("buildAWSConfig: %v", err))
	}

	return cfg
}

// MarshalQueuedPodInfo converts a full QueuedPodInfo into a compact, portable wire format.
func MarshalQueuedPodInfo(q *framework.QueuedPodInfo) ([]byte, error) {
	pb, err := json.Marshal(q.Pod) // just the API object; framework rebuilds the rest
	if err != nil {
		return nil, err
	}
	w := wireQueuedPod{
		PodJSON:  pb,
		Attempts: q.Attempts,
		TS:       q.Timestamp.UnixNano(),
	}
	if !q.BackoffExpiration.IsZero() {
		w.Backoff = q.BackoffExpiration.UnixNano()
	}
	return json.Marshal(&w)
}

// UnmarshalQueuedPodInfo decodes the wire format into a fully-populated QueuedPodInfo
// by letting framework.NewPodInfo rebuild secondary indexes/selectors, etc.
func UnmarshalQueuedPodInfo(b []byte) (*framework.QueuedPodInfo, error) {
	var w wireQueuedPod
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	var pod v1.Pod
	if err := json.Unmarshal(w.PodJSON, &pod); err != nil {
		return nil, err
	}
	pi, err := framework.NewPodInfo(&pod)
	if err != nil {
		return nil, err
	}
	return &framework.QueuedPodInfo{
		PodInfo:           pi,
		Attempts:          w.Attempts,
		Timestamp:         time.Unix(0, w.TS),
		BackoffExpiration: time.Unix(0, w.Backoff),
	}, nil
}

func MarshalPodState(ps *PodState, assumed bool) ([]byte, error) {
	pb, err := json.Marshal(ps.Pod)
	if err != nil {
		return nil, err
	}
	var dl int64
	if ps.Deadline != nil {
		dl = ps.Deadline.UnixNano()
	}
	w := wirePodState{
		PodJSON:      pb,
		DeadlineNsec: dl,
		Assumed:      assumed,
		BindingDone:  ps.BindingFinished,
	}
	return json.Marshal(&w)
}

func UnmarshalPodState(b []byte) (*PodState, bool /*assumed*/, error) {
	var w wirePodState
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, false, err
	}
	var pod v1.Pod
	if err := json.Unmarshal(w.PodJSON, &pod); err != nil {
		return nil, false, err
	}
	var dlPtr *time.Time
	if w.DeadlineNsec != 0 {
		t := time.Unix(0, w.DeadlineNsec)
		dlPtr = &t
	}
	return &PodState{
		Pod:             &pod,
		Deadline:        dlPtr,
		BindingFinished: w.BindingDone,
	}, w.Assumed, nil
}

func marshalNodeInfo(ni *framework.NodeInfo) ([]byte, error) {
	w := wireNodeInfo{Gen: ni.Generation}

	if n := ni.Node(); n != nil {
		nb, err := json.Marshal(n)
		if err != nil {
			return nil, err
		}
		w.NodeJSON = nb
	}

	if len(ni.Pods) > 0 {
		w.PodsJSON = make([][]byte, 0, len(ni.Pods))
		for _, p := range ni.Pods {
			if p == nil || p.Pod == nil {
				continue
			}
			pb, err := json.Marshal(p.Pod)
			if err != nil {
				return nil, err
			}
			w.PodsJSON = append(w.PodsJSON, pb)
		}
	}
	return json.Marshal(&w)
}

func unmarshalNodeInfo(b []byte) (*framework.NodeInfo, error) {
	var w wireNodeInfo
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	ni := framework.NewNodeInfo()

	// reconstruct node
	if len(w.NodeJSON) > 0 {
		var n v1.Node
		if err := json.Unmarshal(w.NodeJSON, &n); err != nil {
			return nil, err
		}
		ni.SetNode(&n)
	}

	// reconstruct pods
	for _, pb := range w.PodsJSON {
		var pod v1.Pod
		if err := json.Unmarshal(pb, &pod); err != nil {
			return nil, err
		}
		ni.AddPod(&pod)
	}

	ni.Generation = w.Gen
	return ni, nil
}
