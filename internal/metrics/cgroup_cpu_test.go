package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cslab-ntua/HPA-Plus/internal/cgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

type cgroupTestTransport func(*http.Request) (*http.Response, error)

func (f cgroupTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCgroupHistoricalUIDDoesNotJoinRecreatedPod(t *testing.T) {
	at := time.Now().Add(-40 * time.Second).UTC()
	finished := at.Add(30 * time.Second)
	row := cgroup.Pod{Namespace: "a", Name: "p", UID: "old-uid", Containers: []cgroup.Container{{Name: "app", ID: "containerd://aaaa", StartedAt: at, FinishedAt: &finished}}}
	for i := 0; i < 15; i++ {
		row.Containers[0].Windows = append(row.Containers[0].Windows, cgroup.Window{From: at.Add(time.Duration(2*i) * time.Second), To: at.Add(time.Duration(2*i+2) * time.Second), CPUSeconds: 1})
	}
	snapshot := cgroup.Snapshot{Version: cgroup.Protocol, Node: "n", CollectedAt: time.Now(), IntervalMillis: 2000, Pods: []cgroup.Pod{row}}
	proxyReads := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/a/pods/p":
			_ = json.NewEncoder(w).Encode(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p", UID: types.UID("new-uid")}, Spec: corev1.PodSpec{NodeName: "n"}})
		case "/api/v1/namespaces/monitor/pods":
			_ = json.NewEncoder(w).Encode(&corev1.PodList{Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "collector", Namespace: "monitor", UID: types.UID("collector-uid")}, Spec: corev1.PodSpec{NodeName: "n"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}}})
		case "/api/v1/namespaces/monitor/pods/collector:19091/proxy/snapshot":
			proxyReads++
			_ = json.NewEncoder(w).Encode(snapshot)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	transport := cgroupTestTransport(func(r *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		return recorder.Result(), nil
	})
	k, err := New(&rest.Config{Host: "http://cgroup-test.invalid", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.ConfigureCgroup(CgroupConfig{Namespace: "monitor"}); err != nil {
		t.Fatal(err)
	}
	old, err := k.GatherCgroupPodResources(context.Background(), "a", "p", "old-uid", at, finished)
	if err != nil || !old.Complete || old.CPUSeconds != 15 || old.RunningPodSeconds != 30 {
		t.Fatalf("explicit historical identity missing %+v %v", old, err)
	}
	new, err := k.GatherCgroupPodResources(context.Background(), "a", "p", "new-uid", at, finished)
	if err != nil || new.Complete || new.CPUSeconds != 0 {
		t.Fatalf("old data joined recreated pod %+v %v", new, err)
	}
	if proxyReads != 1 {
		t.Fatalf("node snapshot not shared: %d proxy reads", proxyReads)
	}
}

func TestAggregateCgroupResourcesRequiresHistoricalNodeInventory(t *testing.T) {
	now := time.Now().UTC()
	from, to := now.Add(-34*time.Second), now.Add(-4*time.Second)
	fixture := func(node, uid string) cgroup.Snapshot {
		pod := cgroup.Pod{Namespace: "a", Name: uid, UID: uid, Labels: map[string]string{"app": "a"}, Containers: []cgroup.Container{{Name: "app", ID: "containerd://" + uid, StartedAt: from.Add(-time.Minute)}}}
		for i := 0; i < 15; i++ {
			pod.Containers[0].Windows = append(pod.Containers[0].Windows, cgroup.Window{From: from.Add(time.Duration(2*i) * time.Second), To: from.Add(time.Duration(2*i+2) * time.Second), CPUSeconds: 1})
		}
		return cgroup.Snapshot{Version: cgroup.Protocol, Node: node, CollectedAt: now, IntervalMillis: 2000, Pods: []cgroup.Pod{pod}}
	}
	snapshots := map[string]cgroup.Snapshot{"c1": fixture("n1", "current"), "c2": fixture("n2", "deleted")}
	phase := corev1.PodRunning
	showSecond := true
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/a/pods":
			_ = json.NewEncoder(w).Encode(&corev1.PodList{Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "current", Namespace: "a", UID: types.UID("current")}, Spec: corev1.PodSpec{NodeName: "n1"}}}})
		case "/api/v1/namespaces/monitor/pods":
			rows := []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "monitor", UID: types.UID("c1")}, Spec: corev1.PodSpec{NodeName: "n1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}}
			if showSecond {
				rows = append(rows, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "c2", Namespace: "monitor", UID: types.UID("c2")}, Spec: corev1.PodSpec{NodeName: "n2"}, Status: corev1.PodStatus{Phase: phase}})
			}
			_ = json.NewEncoder(w).Encode(&corev1.PodList{Items: rows})
		case "/api/v1/namespaces/monitor/pods/c1:19091/proxy/snapshot":
			_ = json.NewEncoder(w).Encode(snapshots["c1"])
		case "/api/v1/namespaces/monitor/pods/c2:19091/proxy/snapshot":
			_ = json.NewEncoder(w).Encode(snapshots["c2"])
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	transport := cgroupTestTransport(func(r *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		return recorder.Result(), nil
	})
	k, err := New(&rest.Config{Host: "http://cgroup-test.invalid", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.ConfigureCgroup(CgroupConfig{Namespace: "monitor"}); err != nil {
		t.Fatal(err)
	}
	r, err := k.GatherCgroupResources(context.Background(), "a", labels.SelectorFromSet(labels.Set{"app": "a"}), from, to)
	if err != nil || !r.Complete || r.CPUSeconds != 30 || r.RunningPodSeconds != 60 || r.PodCount != 2 {
		t.Fatalf("historical node dropped after last pod deletion %+v %v", r, err)
	}
	phase = corev1.PodFailed
	if _, err := k.GatherCgroupResources(context.Background(), "a", labels.Everything(), from, to); err == nil {
		t.Fatal("listed unhealthy collector silently removed its node")
	}
	showSecond = false
	if _, err := k.GatherCgroupResources(context.Background(), "a", labels.Everything(), from, to); err == nil {
		t.Fatal("collector replacement gap erased historical node inventory")
	}
}

func TestCgroupAPIPopulationFenceUsesExactRuntimeIdentitiesAndLifetimes(t *testing.T) {
	from := time.Unix(1700000000, 0)
	to := from.Add(30 * time.Second)
	started := from.Add(-time.Minute)
	memory, optional := uint64(1024), 0.
	current := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "current", Namespace: "a", UID: types.UID("uid-current"), CreationTimestamp: metav1.NewTime(started)},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "app", ContainerID: "containerd://current", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(started)}}}}}}
	inventory := cgroup.Pod{Namespace: "a", Name: "current", UID: "uid-current", Containers: []cgroup.Container{{Name: "app", ID: "containerd://current", StartedAt: started}}}
	newPod := func(start time.Time) corev1.Pod {
		pod := current.DeepCopy()
		pod.Name, pod.UID = "new", types.UID("uid-new")
		pod.Status.ContainerStatuses[0].ContainerID = "containerd://new"
		pod.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(start)
		return *pod
	}
	for _, test := range []struct {
		name     string
		pod      corev1.Pod
		complete bool
	}{
		{"same observed epoch", current, true},
		{"API UID missing from fresh envelope", newPod(started), false},
		{"API startup inside window missing", newPod(from.Add(20 * time.Second)), false},
		{"exact startup after closed window", newPod(to.Add(time.Second)), true},
		{"exact startup at closed endpoint", newPod(to), true},
		{"same UID but new runtime epoch", func() corev1.Pod {
			p := *current.DeepCopy()
			p.Status.ContainerStatuses[0].ContainerID = "containerd://restart"
			return p
		}(), false},
		{"same UID/name but different namespace", func() corev1.Pod { p := current; p.Namespace = "b"; return p }(), false},
		{"same UID different pod name", func() corev1.Pod { p := current; p.Name = "recreated"; return p }(), false},
		{"exact termination before query", func() corev1.Pod {
			p := newPod(started)
			p.Status.Phase = corev1.PodSucceeded
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: "containerd://new", StartedAt: metav1.NewTime(started), FinishedAt: metav1.NewTime(from)}}
			return p
		}(), true},
		{"exact historical runtime within query", func() corev1.Pod {
			p := newPod(started)
			p.Status.Phase = corev1.PodFailed
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: "containerd://new", StartedAt: metav1.NewTime(started), FinishedAt: metav1.NewTime(from.Add(10 * time.Second))}}
			return p
		}(), false},
		{"unknown termination is not absence", func() corev1.Pod {
			p := newPod(started)
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: "containerd://new", StartedAt: metav1.NewTime(started)}}
			return p
		}(), false},
		{"missing previous restart interval", func() corev1.Pod {
			p := newPod(to.Add(time.Second))
			p.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ContainerID: "containerd://previous", StartedAt: metav1.NewTime(started), FinishedAt: metav1.NewTime(from.Add(10 * time.Second))}
			return p
		}(), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			observation := &cgroup.WorkloadObservation{From: from, To: to, Complete: true, InventoryComplete: true, Coverage: 1, CPUSeconds: 15, RunningPodSeconds: 30,
				MemoryBytes: &memory, ThrottledSeconds: &optional, PressureSomeSeconds: &optional, Inventory: []cgroup.Pod{inventory}}
			fenceCgroupAPIPopulation(observation, []corev1.Pod{test.pod})
			if observation.Complete != test.complete || observation.CPUSeconds != 15 || observation.RunningPodSeconds != 30 {
				t.Fatalf("incorrect population fence or invented accounting: %+v", observation)
			}
			if !test.complete && (observation.InventoryComplete || observation.Coverage != 0 || observation.MemoryBytes != nil || observation.ThrottledSeconds != nil || observation.PressureSomeSeconds != nil || len(observation.MissingReasons) == 0) {
				t.Fatalf("incomplete population retained a complete aggregate measurement: %+v", observation)
			}
		})
	}
}

func TestAggregateCgroupResourcesFencesAPIUIDDuringCachedSnapshotLag(t *testing.T) {
	now := time.Now().UTC()
	from, to := now.Add(-34*time.Second), now.Add(-4*time.Second)
	// Kubernetes metav1.Time runtime states serialize at whole-second precision.
	started := from.Add(-time.Minute).Truncate(time.Second)
	row := cgroup.Pod{Namespace: "a", Name: "current", UID: "uid-current", Labels: map[string]string{"app": "a"}, Containers: []cgroup.Container{{Name: "app", ID: "containerd://current", StartedAt: started}}}
	for i := 0; i < 15; i++ {
		row.Containers[0].Windows = append(row.Containers[0].Windows, cgroup.Window{From: from.Add(time.Duration(2*i) * time.Second), To: from.Add(time.Duration(2*i+2) * time.Second), CPUSeconds: 1})
	}
	snapshot := cgroup.Snapshot{Version: cgroup.Protocol, Node: "n", CollectedAt: now, Pods: []cgroup.Pod{row}}
	newUIDKnown, proxyReads := false, 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/a/pods":
			pods := []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "current", Namespace: "a", UID: types.UID("uid-current")}, Spec: corev1.PodSpec{NodeName: "n"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "app", ContainerID: "containerd://current", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(started)}}}}}}}
			if newUIDKnown {
				p := *pods[0].DeepCopy()
				p.Name, p.UID = "new", types.UID("uid-new")
				p.Status.ContainerStatuses[0].ContainerID = "containerd://new"
				pods = append(pods, p)
			}
			_ = json.NewEncoder(w).Encode(&corev1.PodList{Items: pods})
		case "/api/v1/namespaces/monitor/pods":
			_ = json.NewEncoder(w).Encode(&corev1.PodList{Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "collector", Namespace: "monitor", UID: types.UID("collector-uid")}, Spec: corev1.PodSpec{NodeName: "n"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}}})
		case "/api/v1/namespaces/monitor/pods/collector:19091/proxy/snapshot":
			proxyReads++
			_ = json.NewEncoder(w).Encode(snapshot)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	transport := cgroupTestTransport(func(r *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		return recorder.Result(), nil
	})
	k, err := New(&rest.Config{Host: "http://cgroup-test.invalid", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.ConfigureCgroup(CgroupConfig{Namespace: "monitor"}); err != nil {
		t.Fatal(err)
	}
	selector := labels.SelectorFromSet(labels.Set{"app": "a"})
	first, err := k.GatherCgroupResources(context.Background(), "a", selector, from, to)
	if err != nil || !first.Complete || first.Coverage != 1 {
		t.Fatalf("complete initial population rejected: %+v %v", first, err)
	}
	newUIDKnown = true
	second, err := k.GatherCgroupResources(context.Background(), "a", selector, from, to)
	if err != nil || second.Complete || second.Coverage != 0 || second.CPUSeconds != 15 || proxyReads != 1 {
		t.Fatalf("API-known pod omitted by cached fresh collector became full sample: %+v %v reads=%d", second, err, proxyReads)
	}
}
