package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/cslab-ntua/HPA-Plus/internal/cgroup"
	gathered "github.com/jthomperoo/k8shorizmetrics/v2/metrics"
	"github.com/jthomperoo/k8shorizmetrics/v2/metrics/podmetrics"
	resourcemetrics "github.com/jthomperoo/k8shorizmetrics/v2/metrics/resource"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

type CgroupConfig struct {
	Namespace string
	Selector  string
	Port      int
	Window    time.Duration
}

type cachedCgroupSnapshot struct {
	snapshot   cgroup.Snapshot
	receivedAt time.Time
}
type cgroupSource struct {
	config     CgroupConfig
	client     kubernetes.Interface
	mu         sync.Mutex
	cache      map[string]cachedCgroupSnapshot
	knownNodes map[string]time.Time
}

func (k *Kubernetes) ConfigureCgroup(config CgroupConfig) error {
	if config.Namespace == "" {
		return fmt.Errorf("cgroup collector namespace required")
	}
	if config.Selector == "" {
		config.Selector = "app.kubernetes.io/component=cgroup-collector"
	}
	if _, err := labels.Parse(config.Selector); err != nil {
		return fmt.Errorf("invalid cgroup collector selector: %w", err)
	}
	if config.Port == 0 {
		config.Port = 19091
	}
	if config.Port < 1 || config.Port > 65535 {
		return fmt.Errorf("invalid cgroup collector port")
	}
	if config.Window == 0 {
		config.Window = 30 * time.Second
	}
	if config.Window < 2*time.Second || config.Window > time.Minute {
		return fmt.Errorf("invalid cgroup CPU averaging window")
	}
	client, err := kubernetes.NewForConfig(k.config)
	if err != nil {
		return err
	}
	k.cgroup = &cgroupSource{config: config, client: client, cache: map[string]cachedCgroupSnapshot{}, knownNodes: map[string]time.Time{}}
	return nil
}

// CPUHistorySourceIdentity fences persisted training histories across a backend
// change. Counter windows and Metrics API snapshots are different contracts.
func (k *Kubernetes) CPUHistorySourceIdentity() string {
	switch k.cpuHistorySource {
	case "cgroup":
		if k.cgroup != nil {
			return cgroup.Protocol + "/" + k.cgroup.config.Window.String()
		}
		return cgroup.Protocol
	case "kubelet":
		return "kubelet-summary-v1"
	default:
		return "metrics-server-v1"
	}
}

func (s *cgroupSource) read(parent context.Context, nodes map[string]bool, includeInventory ...bool) ([]cgroup.Snapshot, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	// Node-wide snapshots are shared across all workload requests. Serialize only
	// the small bounded read batch so a busy reconciliation cannot multiply reads.
	s.mu.Lock()
	defer s.mu.Unlock()
	collectors, err := s.client.CoreV1().Pods(s.config.Namespace).List(ctx, metav1.ListOptions{LabelSelector: s.config.Selector, Limit: 128})
	if err != nil {
		return nil, fmt.Errorf("collector discovery: %w", err)
	}
	if collectors.Continue != "" {
		return nil, fmt.Errorf("collector discovery exceeds bound")
	}
	byNode := map[string]corev1.Pod{}
	allNodes := nodes == nil || len(includeInventory) > 0 && includeInventory[0]
	if s.knownNodes == nil {
		s.knownNodes = map[string]time.Time{}
	}
	now := time.Now()
	// Expected inventory includes failed/terminating collectors. Remember nodes
	// across pod replacement gaps for longer than retained accounting windows.
	for _, pod := range collectors.Items {
		if pod.Spec.NodeName != "" {
			s.knownNodes[pod.Spec.NodeName] = now
		}
	}
	for node := range nodes {
		s.knownNodes[node] = now
	}
	for node, seen := range s.knownNodes {
		if now.Sub(seen) > 4*time.Minute {
			delete(s.knownNodes, node)
		}
	}
	if len(s.knownNodes) > 256 {
		return nil, fmt.Errorf("cgroup node inventory exceeds bound")
	}
	expected := map[string]bool{}
	for node := range nodes {
		expected[node] = true
	}
	if allNodes {
		for node := range s.knownNodes {
			expected[node] = true
		}
	}
	for _, pod := range collectors.Items {
		if !expected[pod.Spec.NodeName] || pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
			continue
		}
		if _, ok := byNode[pod.Spec.NodeName]; ok {
			return nil, fmt.Errorf("multiple cgroup collectors for node %s", pod.Spec.NodeName)
		}
		byNode[pod.Spec.NodeName] = pod
	}
	var result []cgroup.Snapshot
	for node := range expected {
		pod, ok := byNode[node]
		if !ok {
			return nil, fmt.Errorf("cgroup collector unavailable for node %s", node)
		}
		key := string(pod.UID)
		cached, ok := s.cache[key]
		if ok && time.Since(cached.receivedAt) < time.Second {
			result = append(result, cached.snapshot)
			continue
		}
		reader, err := s.client.CoreV1().RESTClient().Get().Namespace(s.config.Namespace).Resource("pods").Name(pod.Name + ":" + strconv.Itoa(s.config.Port)).SubResource("proxy").Suffix("snapshot").Stream(ctx)
		if err != nil {
			return nil, fmt.Errorf("node cgroup snapshot: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(reader, cgroup.MaxSnapshotBytes+1))
		closeErr := reader.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if len(body) > cgroup.MaxSnapshotBytes {
			return nil, fmt.Errorf("cgroup snapshot exceeds bound")
		}
		var snapshot cgroup.Snapshot
		if err := json.Unmarshal(body, &snapshot); err != nil {
			return nil, fmt.Errorf("invalid cgroup snapshot: %w", err)
		}
		if snapshot.Version != cgroup.Protocol || snapshot.Node != node || snapshot.MissingReason != "" || snapshot.CollectedAt.IsZero() || snapshot.CollectedAt.After(time.Now().Add(time.Second)) || time.Since(snapshot.CollectedAt) > 10*time.Second || len(snapshot.Pods) > cgroup.MaxPods {
			return nil, fmt.Errorf("wrong-node, missing or stale cgroup snapshot")
		}
		s.cache[key] = cachedCgroupSnapshot{snapshot: snapshot, receivedAt: time.Now()}
		result = append(result, snapshot)
	}
	// Collector replacement must not leave node snapshots in memory forever.
	for key, value := range s.cache {
		if time.Since(value.receivedAt) > time.Minute {
			delete(s.cache, key)
		}
	}
	return result, nil
}

func nodesForPods(pods []corev1.Pod) map[string]bool {
	nodes := map[string]bool{}
	for _, pod := range pods {
		if pod.Spec.NodeName != "" {
			nodes[pod.Spec.NodeName] = true
		}
	}
	return nodes
}

// GatherCgroupResources returns aligned CPU-seconds and pod-seconds, with raw
// windows and coverage. Callers must use a closed interval available from every
// participating node (normally ending four seconds before collection).
func (k *Kubernetes) GatherCgroupResources(ctx context.Context, namespace string, selector labels.Selector, from, to time.Time) (*cgroup.WorkloadObservation, error) {
	if k.cgroup == nil {
		return nil, fmt.Errorf("cgroup collector is not configured")
	}
	pods, err := k.cgroup.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String(), Limit: 2048})
	if err != nil {
		return nil, err
	}
	if pods.Continue != "" {
		return nil, fmt.Errorf("workload pod bound exceeded")
	}
	snapshots, err := k.cgroup.read(ctx, nodesForPods(pods.Items), true)
	if err != nil {
		return nil, err
	}
	observation, err := cgroup.Integrate(snapshots, namespace, selector, from, to, time.Now())
	if err == nil {
		fenceCgroupAPIPopulation(observation, pods.Items)
	}
	return observation, err
}

// A node informer's fresh envelope can still lag an API-known pod or restart.
// Missing those identities must not turn the observed subset into full fleet
// coverage. Partial energy remains real, but is unusable as a fleet sample.
func fenceCgroupAPIPopulation(observation *cgroup.WorkloadObservation, pods []corev1.Pod) {
	if observation == nil {
		return
	}
	inventory := observation.Inventory
	if len(inventory) == 0 {
		inventory = observation.Pods
	}
	known := make(map[string]cgroup.Pod, len(inventory))
	for _, pod := range inventory {
		known[pod.UID] = pod
	}
	missing := func(reason string) {
		observation.Complete = false
		observation.InventoryComplete = false
		// Full-population coverage is not established. Do not invent an exact
		// missing CPU quantity or use the healthy subset's coverage percentage.
		observation.Coverage = 0
		observation.MemoryBytes, observation.ThrottledSeconds = nil, nil
		observation.PressureSomeSeconds, observation.PressureFullSeconds = nil, nil
		observation.MemorySourceAt = time.Time{}
		if len(observation.MissingReasons) < 32 {
			observation.MissingReasons = append(observation.MissingReasons, reason)
		}
	}
	for _, pod := range pods {
		if !pod.CreationTimestamp.IsZero() && !pod.CreationTimestamp.Time.Before(observation.To) {
			continue
		}
		row, found := known[string(pod.UID)]
		found = found && row.Namespace == pod.Namespace && row.Name == pod.Name
		hasRuntimeState := false
		requireEpoch := func(name, id string, start time.Time, finished *time.Time) {
			if !start.IsZero() && !start.Before(observation.To) || finished != nil && !finished.After(observation.From) {
				return
			}
			if !found {
				missing(pod.Name + ": API-known runtime pod missing from collector inventory")
				return
			}
			for _, container := range row.Containers {
				if container.Name == name && id != "" && container.ID == id && !container.StartedAt.IsZero() && (start.IsZero() || container.StartedAt.Equal(start)) {
					return
				}
			}
			missing(pod.Name + "/" + name + ": API-known runtime epoch missing from collector inventory")
		}
		statuses := append(append([]corev1.ContainerStatus{}, pod.Status.ContainerStatuses...), pod.Status.InitContainerStatuses...)
		for _, status := range statuses {
			if state := status.State.Running; state != nil {
				hasRuntimeState = true
				requireEpoch(status.Name, status.ContainerID, state.StartedAt.Time, nil)
			}
			if state := status.State.Terminated; state != nil {
				hasRuntimeState = true
				id := state.ContainerID
				if id == "" {
					id = status.ContainerID
				}
				var end *time.Time
				if !state.FinishedAt.IsZero() {
					value := state.FinishedAt.Time
					end = &value
				}
				requireEpoch(status.Name, id, state.StartedAt.Time, end)
			}
			if state := status.LastTerminationState.Terminated; state != nil {
				hasRuntimeState = true
				var end *time.Time
				if !state.FinishedAt.IsZero() {
					value := state.FinishedAt.Time
					end = &value
				}
				requireEpoch(status.Name, state.ContainerID, state.StartedAt.Time, end)
			}
		}
		if !hasRuntimeState && (pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodUnknown) && !found {
			missing(pod.Name + ": API running pod has no collector runtime identity")
		}
	}
}

// GatherCgroupPodResources binds native telemetry accounting to one exact pod
// UID. Reusing a pod name after recreation cannot attach old CPU to new traffic.
func (k *Kubernetes) GatherCgroupPodResources(ctx context.Context, namespace, podName, podUID string, from, to time.Time) (*cgroup.WorkloadObservation, error) {
	if k.cgroup == nil || podUID == "" {
		return nil, fmt.Errorf("cgroup collector and pod UID required")
	}
	pod, err := k.cgroup.client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	var nodes map[string]bool
	if err == nil && string(pod.UID) == podUID && pod.Spec.NodeName != "" {
		nodes = map[string]bool{pod.Spec.NodeName: true}
	}
	// Explicit historical UIDs remain queryable after deletion/recreation. The
	// selector caller must retain its previously owned UID; no new UID is adopted.
	snapshots, err := k.cgroup.read(ctx, nodes)
	if err != nil {
		return nil, err
	}
	for i := range snapshots {
		var rows []cgroup.Pod
		for _, row := range snapshots[i].Pods {
			if row.Namespace == namespace && row.Name == podName && row.UID == podUID {
				rows = append(rows, row)
			}
		}
		snapshots[i].Pods = rows
	}
	observation, err := cgroup.Integrate(snapshots, namespace, labels.Everything(), from, to, time.Now())
	if err == nil && pod != nil && string(pod.UID) == podUID {
		fenceCgroupAPIPopulation(observation, []corev1.Pod{*pod})
	}
	return observation, err
}

// historyStartGap is how long after a container start its first counter read
// may come; the collector reads every second and needs two reads per window.
// A container that stops is likewise covered when its last read came at most
// this long before it stopped.
const historyStartGap = 3 * time.Second

func (s *cgroupSource) podMetrics(ctx context.Context, pods *rawPodLister, resource corev1.ResourceName, namespace string, selector labels.Selector) (podmetrics.MetricsInfo, time.Time, error) {
	values, stamp, _, err := s.podMetricsMode(ctx, pods, resource, namespace, selector, false, s.config.Window)
	return values, stamp, err
}

// podMetricsMode with history set measures what the deployment used over the
// window rather than what each current pod can serve: a container that started
// inside the window counts from its start, a pod being deleted counts until its
// containers stopped, and a pod that used no measurable CPU in the window (not
// started by its end, or stopped without reads covering its part) is returned
// in excluded instead of as missing. The reactive calculation keeps excluding
// such pods (history unset).
func (s *cgroupSource) podMetricsMode(ctx context.Context, pods *rawPodLister, resource corev1.ResourceName, namespace string, selector labels.Selector, history bool, window time.Duration) (podmetrics.MetricsInfo, time.Time, map[string]bool, error) {
	if pods == nil {
		return nil, time.Time{}, nil, fmt.Errorf("cgroup CPU needs current pod identities")
	}
	objects, err := pods.readPods(namespace, selector)
	if err != nil {
		return nil, time.Time{}, nil, err
	}
	var typed []corev1.Pod
	for _, object := range objects {
		var pod corev1.Pod
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &pod); err != nil {
			return nil, time.Time{}, nil, err
		}
		typed = append(typed, pod)
	}
	snapshots, err := s.read(ctx, nodesForPods(typed))
	if err != nil {
		return nil, time.Time{}, nil, err
	}
	rows := map[string]cgroup.Pod{}
	for _, snapshot := range snapshots {
		for _, row := range snapshot.Pods {
			if row.Namespace == namespace {
				if _, ok := rows[row.UID]; ok {
					return nil, time.Time{}, nil, fmt.Errorf("duplicate cgroup pod identity")
				}
				rows[row.UID] = row
			}
		}
	}
	result := podmetrics.MetricsInfo{}
	unresolved := map[string]corev1.Pod{}
	startGap := time.Duration(0)
	if history {
		startGap = historyStartGap
	}
	var stopping []int
	for i, pod := range typed {
		if history && pod.DeletionTimestamp != nil {
			// Measured below over the window its running peers define.
			stopping = append(stopping, i)
			continue
		}
		unresolved[pod.Name] = pod
		row, ok := rows[string(pod.UID)]
		if !ok || row.Name != pod.Name || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		var view struct {
			Spec requestSpec `json:"spec"`
		}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(objects[i].Object, &view); err != nil {
			return nil, time.Time{}, nil, err
		}
		expected, err := view.Spec.runningContainers()
		if err != nil {
			return nil, time.Time{}, nil, err
		}
		ids := map[string]string{}
		for _, status := range append(append([]corev1.ContainerStatus{}, pod.Status.ContainerStatuses...), pod.Status.InitContainerStatuses...) {
			if status.State.Running != nil {
				ids[status.Name] = status.ContainerID
			}
		}
		containers := map[string]cgroup.Container{}
		for _, container := range row.Containers {
			if container.ID != "" && ids[container.Name] == container.ID {
				containers[container.Name] = container
			}
		}
		var until time.Time
		complete := true
		for _, expected := range expected {
			container, ok := containers[expected.Name]
			if !ok || len(container.Windows) == 0 {
				complete = false
				break
			}
			last := container.Windows[len(container.Windows)-1]
			if time.Since(last.To) > 10*time.Second || last.To.After(time.Now().Add(time.Second)) {
				complete = false
				break
			}
			if until.IsZero() || last.To.Before(until) {
				until = last.To
			}
		}
		if !complete || until.IsZero() {
			continue
		}
		var totalCPU float64
		var memory uint64
		for _, expected := range expected {
			container := containers[expected.Name]
			if resource == corev1.ResourceMemory {
				last := container.Windows[len(container.Windows)-1]
				if last.MemoryBytes == nil || math.MaxUint64-memory < *last.MemoryBytes {
					complete = false
					break
				}
				memory += *last.MemoryBytes
				continue
			}
			cpu, covered, err := containerCPU(container, until.Add(-window), until, startGap)
			if err != nil {
				return nil, time.Time{}, nil, err
			}
			if !covered {
				complete = false
				break
			}
			totalCPU += cpu
		}
		if !complete {
			continue
		}
		if err := checkTimestamp(ctx, until); err != nil {
			return nil, time.Time{}, nil, err
		}
		var value int64
		if resource == corev1.ResourceMemory {
			if memory > math.MaxInt64/1000 {
				return nil, time.Time{}, nil, fmt.Errorf("memory metric overflow")
			}
			value = int64(memory) * 1000
		} else {
			milli := math.Ceil(totalCPU / window.Seconds() * 1000)
			if math.IsNaN(milli) || math.IsInf(milli, 0) || milli < 0 || milli > math.MaxInt64 {
				return nil, time.Time{}, nil, fmt.Errorf("CPU metric overflow")
			}
			value = int64(milli)
		}
		result[pod.Name] = podmetrics.Metric{Timestamp: until, Window: window, Value: value}
		delete(unresolved, pod.Name)
	}
	stamp := oldestPodTimestamp(result)
	var excluded map[string]bool
	if history && !stamp.IsZero() {
		excluded = map[string]bool{}
		for name, pod := range unresolved {
			if !startedBefore(pod, stamp.Add(-historyStartGap)) {
				excluded[name] = true
			}
		}
		for _, i := range stopping {
			pod := typed[i]
			cpu, ok, err := stoppingPodCPU(pod, objects[i], rows[string(pod.UID)], stamp.Add(-window), stamp)
			if err != nil {
				return nil, time.Time{}, nil, err
			}
			if !ok {
				excluded[pod.Name] = true
				continue
			}
			result[pod.Name] = podmetrics.Metric{Timestamp: stamp, Window: window, Value: int64(math.Ceil(cpu / window.Seconds() * 1000))}
		}
	}
	return result, stamp, excluded, nil
}

// stoppingPodCPU measures a pod that is being deleted over [from, until]. Each
// container counts until it stopped; it used no CPU afterwards. It reports
// false when the pod did not run in the window or its reads do not cover its
// part of it: such a pod's CPU is unknown rather than zero.
func stoppingPodCPU(pod corev1.Pod, object unstructured.Unstructured, row cgroup.Pod, from, until time.Time) (float64, bool, error) {
	if row.UID != string(pod.UID) || row.Name != pod.Name {
		return 0, false, nil
	}
	var view struct {
		Spec requestSpec `json:"spec"`
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &view); err != nil {
		return 0, false, err
	}
	expected, err := view.Spec.runningContainers()
	if err != nil {
		return 0, false, err
	}
	ids := map[string]string{}
	for _, status := range append(append([]corev1.ContainerStatus{}, pod.Status.ContainerStatuses...), pod.Status.InitContainerStatuses...) {
		switch {
		case status.State.Running != nil:
			ids[status.Name] = status.ContainerID
		case status.State.Terminated != nil && status.State.Terminated.ContainerID != "":
			ids[status.Name] = status.State.Terminated.ContainerID
		case status.State.Terminated != nil:
			ids[status.Name] = status.ContainerID
		}
	}
	total, ran := 0., false
	for _, want := range expected {
		var container *cgroup.Container
		for i := range row.Containers {
			if c := &row.Containers[i]; c.Name == want.Name && c.ID != "" && c.ID == ids[want.Name] {
				container = c
			}
		}
		if container == nil || len(container.Windows) == 0 {
			return 0, false, nil
		}
		end := until
		stopped := container.FinishedAt
		if stopped == nil {
			stopped = container.EndUpperBound
		}
		if stopped != nil && stopped.Before(until) {
			last := container.Windows[len(container.Windows)-1].To
			if stopped.Sub(last) > historyStartGap {
				return 0, false, nil
			}
			if !last.After(from) {
				continue // stopped before the window
			}
			end = last
		}
		cpu, covered, err := containerCPU(*container, from, end, historyStartGap)
		if err != nil || !covered {
			return 0, false, err
		}
		total, ran = total+cpu, true
	}
	return total, ran, nil
}

// containerCPU integrates measured CPU-seconds over [from, until] and reports
// whether real reads cover it. With a positive startGap, a container that
// started inside the window is covered from its start, provided its first read
// follows the start by at most startGap; startup CPU before that read is lost.
func containerCPU(container cgroup.Container, from, until time.Time, startGap time.Duration) (float64, bool, error) {
	cursor, opening := from, false
	if startGap > 0 && container.StartedAt.After(from) {
		cursor, opening = container.StartedAt, true
	}
	total := 0.
	var previousTo time.Time
	for _, window := range container.Windows {
		if !window.To.After(window.From) || window.To.Sub(window.From) > 10*time.Second || window.From.Before(container.StartedAt) || !previousTo.IsZero() && window.From.Before(previousTo) || window.CPUSeconds < 0 || math.IsNaN(window.CPUSeconds) || math.IsInf(window.CPUSeconds, 0) {
			return 0, false, fmt.Errorf("invalid cgroup CPU counter window")
		}
		previousTo = window.To
		left, right := from, until
		if window.From.After(left) {
			left = window.From
		}
		if window.To.Before(right) {
			right = window.To
		}
		if !right.After(left) {
			continue
		}
		if left.After(cursor.Add(time.Millisecond)) && !(opening && left.Sub(cursor) <= startGap) {
			return total, false, nil
		}
		opening = false
		total += window.CPUSeconds * right.Sub(left).Seconds() / window.To.Sub(window.From).Seconds()
		cursor = right
	}
	return total, !cursor.Before(until.Add(-time.Millisecond)), nil
}

// startedBefore reports whether any of the pod's containers was running before t.
// A pod without such a container used no CPU in a window that ends at t.
func startedBefore(pod corev1.Pod, t time.Time) bool {
	for _, status := range append(append([]corev1.ContainerStatus{}, pod.Status.ContainerStatuses...), pod.Status.InitContainerStatuses...) {
		for _, started := range []metav1.Time{runningSince(status.State), runningSince(status.LastTerminationState)} {
			if !started.IsZero() && started.Time.Before(t) {
				return true
			}
		}
	}
	return false
}

func runningSince(state corev1.ContainerState) metav1.Time {
	if state.Running != nil {
		return state.Running.StartedAt
	}
	if state.Terminated != nil {
		return state.Terminated.StartedAt
	}
	return metav1.Time{}
}

func (k *Kubernetes) gatherCgroupCPUHistory(ctx context.Context, spec autoscalingv2.MetricSpec, namespace string, selector labels.Selector) (*gathered.Metric, error) {
	if k.cgroup == nil {
		return nil, fmt.Errorf("cgroup CPU source not configured")
	}
	return k.gatherCgroupCPUWindow(ctx, spec, namespace, selector, k.cgroup.config.Window)
}

// gatherCgroupCPUWindow measures what the workload used over the window that
// ends at the collectors' latest reads, with CPU-history population rules.
func (k *Kubernetes) gatherCgroupCPUWindow(ctx context.Context, spec autoscalingv2.MetricSpec, namespace string, selector labels.Selector, window time.Duration) (*gathered.Metric, error) {
	if k.cgroup == nil {
		return nil, fmt.Errorf("cgroup CPU source not configured")
	}
	dyn, err := dynamic.NewForConfig(k.config)
	if err != nil {
		return nil, err
	}
	lister := &rawPodLister{client: dyn, ctx: ctx, resources: map[corev1.ResourceName]bool{corev1.ResourceCPU: true}, stopping: true}
	objects, err := lister.readPods(namespace, selector)
	if err != nil {
		return nil, err
	}
	values, stamp, excluded, err := k.cgroup.podMetricsMode(ctx, lister, corev1.ResourceCPU, namespace, selector, true, window)
	if err != nil {
		return nil, err
	}
	// A pod without measurable CPU in the window (not started by its end, or
	// stopped without reads covering its part) is neither part of this
	// window's population nor missing from it.
	result := &resourcemetrics.Metric{PodMetricsInfo: values, Timestamp: stamp, Requests: map[string]int64{}, MissingPods: sets.NewString(), IgnoredPods: sets.NewString(), TotalPods: len(objects) - len(excluded)}
	for i := range objects {
		pod, err := projectPod(&objects[i], lister.resources)
		if err != nil {
			return nil, err
		}
		if excluded[pod.Name] {
			continue
		}
		result.Requests[pod.Name] = pod.Spec.Containers[0].Resources.Requests.Cpu().MilliValue()
		if _, ok := values[pod.Name]; !ok {
			result.MissingPods.Insert(pod.Name)
		}
	}
	result.ReadyPodCount = int64(len(values))
	return &gathered.Metric{Spec: spec, Resource: result}, nil
}
