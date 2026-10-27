package metrics

import (
	"math"
	"testing"
	"time"

	"github.com/cslab-ntua/HPA-Plus/internal/cgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func secondWindows(start, from, to time.Time, cpu float64) cgroup.Container {
	c := cgroup.Container{Name: "app", ID: "c1", StartedAt: start}
	for t := from; t.Before(to); t = t.Add(time.Second) {
		c.Windows = append(c.Windows, cgroup.Window{From: t, To: t.Add(time.Second), CPUSeconds: cpu})
	}
	return c
}

func TestContainerCPUCountsAContainerFromItsStartOnlyForHistory(t *testing.T) {
	end := time.Date(2026, 10, 21, 13, 29, 30, 0, time.UTC)
	from := end.Add(-30 * time.Second)
	// Started 12 s before the window end; first read 1 s later; half a core since.
	started := end.Add(-12 * time.Second)
	c := secondWindows(started, started.Add(time.Second), end, .5)
	if _, covered, err := containerCPU(c, from, end, 0); err != nil || covered {
		t.Fatalf("reactive mode must keep treating a partial container as uncovered: %v %v", covered, err)
	}
	cpu, covered, err := containerCPU(c, from, end, historyStartGap)
	if err != nil || !covered || math.Abs(cpu-5.5) > 1e-9 {
		t.Fatalf("history mode must count 11 s at half a core: %v %v %v", cpu, covered, err)
	}
	late := secondWindows(started, started.Add(5*time.Second), end, .5)
	if _, covered, _ := containerCPU(late, from, end, historyStartGap); covered {
		t.Fatal("a first read 5 s after start leaves unmeasured CPU and must stay uncovered")
	}
	gap := secondWindows(started, started.Add(time.Second), end, .5)
	gap.Windows = append(gap.Windows[:3], gap.Windows[6:]...)
	if _, covered, _ := containerCPU(gap, from, end, historyStartGap); covered {
		t.Fatal("a gap between reads must stay uncovered")
	}
	full := secondWindows(from.Add(-time.Hour), from, end, 1)
	if cpu, covered, _ := containerCPU(full, from, end, historyStartGap); !covered || math.Abs(cpu-30) > 1e-9 {
		t.Fatalf("a container running the whole window is unchanged: %v %v", cpu, covered)
	}
}

func TestStartedBeforeSeparatesNotYetRunningPods(t *testing.T) {
	end := time.Date(2026, 10, 21, 13, 29, 30, 0, time.UTC)
	running := func(at time.Time) corev1.Pod {
		return corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Time{Time: at}}}}}}}
	}
	pending := corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{Name: "app",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}}}
	if startedBefore(pending, end) {
		t.Fatal("a pending pod used no CPU in the window")
	}
	if !startedBefore(running(end.Add(-10*time.Second)), end) {
		t.Fatal("a pod running during the window must be measured, never dropped")
	}
	if startedBefore(running(end.Add(time.Second)), end) {
		t.Fatal("a pod started after the window end used no CPU in it")
	}
}

func TestStoppingPodCPUCountsADeletedPodUntilItStopped(t *testing.T) {
	// r14 peak 3: two pods deleted as the 16:12:29 window closed had served all
	// of it, but the sampler left them out and the window read 1.50 of 2.55 cores.
	end := time.Date(2026, 10, 21, 16, 12, 29, 0, time.UTC)
	from := end.Add(-30 * time.Second)
	deleted := metav1.NewTime(end.Add(time.Second))
	object := unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{
		"containers": []interface{}{map[string]interface{}{"name": "app"}}}}}
	pod := func(state corev1.ContainerState) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", UID: "u", DeletionTimestamp: &deleted},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", ContainerID: "c1", State: state}}}}
	}
	draining := pod(corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
	exited := pod(corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: "c1"}})
	row := func(c cgroup.Container) cgroup.Pod {
		return cgroup.Pod{Name: "p", UID: "u", Containers: []cgroup.Container{c}}
	}

	running := secondWindows(from.Add(-time.Hour), from, end, .5)
	if cpu, ok, err := stoppingPodCPU(draining, object, row(running), from, end); err != nil || !ok || math.Abs(cpu-15) > 1e-9 {
		t.Fatalf("a draining pod must count its whole window: %v %v %v", cpu, ok, err)
	}
	stopped := secondWindows(from.Add(-time.Hour), from, end.Add(-10*time.Second), .5)
	at := end.Add(-9 * time.Second)
	stopped.FinishedAt = &at
	if cpu, ok, err := stoppingPodCPU(exited, object, row(stopped), from, end); err != nil || !ok || math.Abs(cpu-10) > 1e-9 {
		t.Fatalf("a pod that stopped mid-window must count until its last read: %v %v %v", cpu, ok, err)
	}
	gone := stopped
	upper := end.Add(-9 * time.Second)
	gone.FinishedAt, gone.EndUpperBound = nil, &upper
	if _, ok, _ := stoppingPodCPU(draining, object, row(gone), from, end); !ok {
		t.Fatal("a vanished cgroup bounds the end like a reported exit")
	}
	late := stopped
	lateAt := end.Add(-5 * time.Second)
	late.FinishedAt = &lateAt
	if _, ok, _ := stoppingPodCPU(exited, object, row(late), from, end); ok {
		t.Fatal("5 s without reads before the exit is unmeasured CPU, not zero")
	}
	earlier := secondWindows(from.Add(-time.Hour), from.Add(-20*time.Second), from.Add(-10*time.Second), .5)
	before := from.Add(-9 * time.Second)
	earlier.FinishedAt = &before
	if _, ok, _ := stoppingPodCPU(exited, object, row(earlier), from, end); ok {
		t.Fatal("a pod that stopped before the window is not part of it")
	}
	other := running
	other.ID = "c0"
	if _, ok, _ := stoppingPodCPU(draining, object, row(other), from, end); ok {
		t.Fatal("reads of another container epoch must not be counted")
	}
}
