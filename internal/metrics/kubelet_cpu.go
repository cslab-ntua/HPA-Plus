package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	gathered "github.com/jthomperoo/k8shorizmetrics/v2/metrics"
	"github.com/jthomperoo/k8shorizmetrics/v2/metrics/podmetrics"
	resourcemetrics "github.com/jthomperoo/k8shorizmetrics/v2/metrics/resource"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type kubeletContainerCPU struct {
	Name      string      `json:"name"`
	StartTime metav1.Time `json:"startTime"`
	CPU       *struct {
		Time      metav1.Time `json:"time"`
		NanoCores *uint64     `json:"usageNanoCores"`
	} `json:"cpu"`
}

type kubeletPodCPU struct {
	Ref struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		UID       string `json:"uid"`
	} `json:"podRef"`
	Containers []kubeletContainerCPU `json:"containers"`
}

type cpuContainerInstance struct {
	StartedAt        time.Time
	PreviousFinished time.Time
	RestartCount     int32
}

// Namespace and pod UID are checked against a current selector list. Native
// reactive metrics are never replaced. A missing rate/container remains missing,
// including the first kubelet observation of a new container; zero is valid.
func (k *Kubernetes) gatherKubeletCPUHistory(parent context.Context, spec autoscalingv2.MetricSpec, namespace string, selector labels.Selector) (*gathered.Metric, error) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	config := rest.CopyConfig(k.config)
	config.Wrap(func(base http.RoundTripper) http.RoundTripper { return &contextTransport{parent: ctx, base: base} })
	if config.RateLimiter != nil {
		config.RateLimiter = &contextLimiter{RateLimiter: config.RateLimiter, parent: ctx}
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	lister := &rawPodLister{client: dyn, ctx: ctx, resources: map[corev1.ResourceName]bool{corev1.ResourceCPU: true}}
	objects, err := lister.readPods(namespace, selector)
	if err != nil {
		return nil, err
	}
	if len(objects) == 0 {
		return nil, fmt.Errorf("no pods available for CPU history")
	}
	result := &resourcemetrics.Metric{PodMetricsInfo: podmetrics.MetricsInfo{}, Requests: map[string]int64{}, MissingPods: sets.NewString(), IgnoredPods: sets.NewString(), TotalPods: len(objects)}
	nodes := map[string][]kubeletPodCPU{}
	for _, obj := range objects {
		pod, err := projectPod(&obj, lister.resources)
		if err != nil {
			return nil, err
		}
		request := pod.Spec.Containers[0].Resources.Requests.Cpu().MilliValue()
		result.Requests[pod.Name] = request
		if pod.Spec.NodeName == "" || pod.Status.Phase != corev1.PodRunning {
			result.MissingPods.Insert(pod.Name)
			continue
		}
		if _, ok := nodes[pod.Spec.NodeName]; !ok {
			body, err := client.CoreV1().RESTClient().Get().AbsPath("/api/v1/nodes/" + pod.Spec.NodeName + "/proxy/stats/summary").Do(ctx).Raw()
			if err != nil {
				return nil, fmt.Errorf("CPU history node read unavailable: %w", err)
			}
			var summary struct {
				Pods []kubeletPodCPU `json:"pods"`
			}
			if err := json.Unmarshal(body, &summary); err != nil {
				return nil, err
			}
			nodes[pod.Spec.NodeName] = summary.Pods
		}
		var view struct {
			Spec requestSpec `json:"spec"`
		}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &view); err != nil {
			return nil, err
		}
		containers, err := view.Spec.runningContainers()
		if err != nil {
			return nil, err
		}
		running := map[string]cpuContainerInstance{}
		for _, status := range append(pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses...) {
			if status.State.Running != nil {
				instance := cpuContainerInstance{StartedAt: status.State.Running.StartedAt.Time, RestartCount: status.RestartCount}
				if status.LastTerminationState.Terminated != nil {
					instance.PreviousFinished = status.LastTerminationState.Terminated.FinishedAt.Time
				}
				running[status.Name] = instance
			}
		}
		expected := map[string]cpuContainerInstance{}
		allRunning := true
		for _, container := range containers {
			started, ok := running[container.Name]
			if !ok || started.StartedAt.IsZero() {
				allRunning = false
				break
			}
			expected[container.Name] = started
		}
		if !allRunning {
			result.MissingPods.Insert(pod.Name)
			continue
		}
		var value *podmetrics.Metric
		for _, row := range nodes[pod.Spec.NodeName] {
			if pod.UID != "" && row.Ref.Namespace == namespace && row.Ref.Name == pod.Name && row.Ref.UID == string(pod.UID) {
				value, err = kubeletCPUValue(ctx, row.Containers, expected)
				if err != nil {
					return nil, err
				}
				break
			}
		}
		if value == nil {
			result.MissingPods.Insert(pod.Name)
			continue
		}
		result.PodMetricsInfo[pod.Name] = *value
	}
	result.ReadyPodCount = int64(len(result.PodMetricsInfo))
	result.Timestamp = oldestPodTimestamp(result.PodMetricsInfo)
	return &gathered.Metric{Spec: spec, Resource: result}, nil
}

func kubeletCPUValue(ctx context.Context, rows []kubeletContainerCPU, expected map[string]cpuContainerInstance) (*podmetrics.Metric, error) {
	seen := map[string]bool{}
	var total int64
	var oldest time.Time
	for _, row := range rows {
		started, wanted := expected[row.Name]
		if !wanted {
			continue
		}
		if seen[row.Name] || row.CPU == nil || row.CPU.NanoCores == nil || row.CPU.Time.IsZero() {
			return nil, nil
		}
		seen[row.Name] = true
		if err := checkTimestamp(ctx, row.CPU.Time.Time); err != nil {
			return nil, err
		}
		if row.StartTime.IsZero() || row.StartTime.After(row.CPU.Time.Time) || row.CPU.Time.Before(&metav1.Time{Time: started.StartedAt}) {
			return nil, nil
		}
		// Kubelet records cgroup creation; the runtime records process start.
		// Cgroup creation can legitimately precede API StartedAt by a second.
		// A pod UID with zero restarts has no earlier container instance. After
		// restart, require the cgroup to follow the previous termination; do
		// not use a guessed timestamp tolerance to accept an old instance.
		if started.RestartCount > 0 && (started.PreviousFinished.IsZero() || row.StartTime.Time.Before(started.PreviousFinished)) {
			return nil, nil
		}
		nano := *row.CPU.NanoCores
		milli := int64(nano / 1000000)
		if nano%1000000 != 0 {
			milli++
		}
		var err error
		total, err = addMetric(total, milli)
		if err != nil {
			return nil, err
		}
		if oldest.IsZero() || row.CPU.Time.Before(&metav1.Time{Time: oldest}) {
			oldest = row.CPU.Time.Time
		}
	}
	if len(expected) == 0 || len(seen) != len(expected) {
		return nil, nil
	}
	return &podmetrics.Metric{Timestamp: oldest, Value: total}, nil
}
