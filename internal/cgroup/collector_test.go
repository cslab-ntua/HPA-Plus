package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

func TestCollectorLifecycleDoesNotReuseCounterAcrossRestart(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory"), 0644); err != nil {
		t.Fatal(err)
	}
	uid := "11111111-2222-3333-4444-555555555555"
	id := strings.Repeat("a", 64)
	write := func(id string, usage int) {
		path := filepath.Join(root, "kubepods", "pod"+uid, id)
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "cpu.stat"), []byte(fmt.Sprintf("usage_usec %d\n", usage)), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(id, 1000000)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p", UID: types.UID(uid), CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)), Labels: map[string]string{"app": "a"}}, Spec: corev1.PodSpec{NodeName: "n", Containers: []corev1.Container{{Name: "app"}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "app", ContainerID: "containerd://" + id, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-time.Minute))}}}}}}
	c, err := NewCollector(root, "n", 2*time.Second, 3*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c.Collect([]*corev1.Pod{pod}, time.Now())
	s := c.Snapshot()
	if len(s.Pods) != 1 || len(s.Pods[0].Containers[0].Windows) != 0 || s.Pods[0].Containers[0].MissingReason == "" {
		t.Fatalf("first read fabricated a rate %+v", s)
	}
	write(id, 2000000)
	c.Collect([]*corev1.Pod{pod}, time.Now())
	s = c.Snapshot()
	if len(s.Pods[0].Containers[0].Windows) != 1 || s.Pods[0].Containers[0].Windows[0].CPUSeconds != 1 {
		t.Fatalf("second delta missing %+v", s)
	}
	newID := strings.Repeat("b", 64)
	write(newID, 10)
	pod.Status.ContainerStatuses[0].ContainerID = "containerd://" + newID
	finished := metav1.NewTime(time.Now())
	pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: "containerd://" + id, FinishedAt: finished}}
	c.Collect([]*corev1.Pod{pod}, time.Now())
	s = c.Snapshot()
	if len(s.Pods[0].Containers) != 2 {
		t.Fatalf("restart identity missing %+v", s)
	}
	for _, row := range s.Pods[0].Containers {
		if row.ID == "containerd://"+newID && len(row.Windows) != 0 {
			t.Fatal("restart reused old counter")
		}
		if row.ID == "containerd://"+id && row.FinishedAt == nil {
			t.Fatal("old epoch stayed active")
		}
		if row.ID == "containerd://"+id && !row.FinishedAt.Equal(finished.Time) {
			t.Fatal("restart did not retain exact previous runtime termination")
		}
	}
	c.Collect(nil, time.Now())
	s = c.Snapshot()
	if len(s.Pods) != 1 || s.Pods[0].DeletedAt == nil {
		t.Fatal("deleted pod history not retained")
	}
	// Snapshot ownership is independent from subsequent callers.
	s.Pods[0].Labels["app"] = "mutated"
	if c.Snapshot().Pods[0].Labels["app"] != "a" {
		t.Fatal("snapshot leaked internal label map")
	}
	c.Collect(nil, time.Now().Add(4*time.Minute))
	if len(c.Snapshot().Pods) != 0 {
		t.Fatal("deleted histories exceeded retention")
	}
}

func TestDisappearedCgroupRetainsUnmeasuredTail(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	c, err := NewCollector(root, "n", 2*time.Second, 3*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	from := now.Add(-10 * time.Second)
	last := now.Add(-2 * time.Second)
	container := &containerState{row: Container{Name: "app", ID: "containerd://" + strings.Repeat("a", 64), StartedAt: from, Windows: []Window{{From: from, To: last, CPUSeconds: 4}}}, previous: &Counters{At: last, Usage: 4000000}, path: filepath.Join(root, "previously-resolved-now-gone")}
	c.pods["uid-a"] = &podState{row: Pod{Name: "p", Namespace: "a", UID: "uid-a"}, containers: map[string]*containerState{"app/old": container}, lastSeen: now.Add(-time.Second)}
	c.Collect(nil, now)
	snapshot := c.Snapshot()
	row := snapshot.Pods[0].Containers[0]
	if row.FinishedAt != nil || row.EndUpperBound == nil || !row.EndUpperBound.After(last) {
		t.Fatalf("last read was invented as termination %+v", row)
	}
	result, err := Integrate([]Snapshot{snapshot}, "a", labels.Everything(), from, *row.EndUpperBound, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if result.Complete || result.Coverage >= 1 || result.CPUSeconds != 4 || result.RunningPodSeconds <= 8 {
		t.Fatalf("unknown tail falsely complete %+v", result)
	}
	// A caller evaluating only the unknown tail must not see measured zero CPU.
	tail, err := Integrate([]Snapshot{snapshot}, "a", labels.Everything(), last, *row.EndUpperBound, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if tail.Complete || tail.Coverage != 0 || len(tail.MissingReasons) == 0 {
		t.Fatalf("unknown tail became zero %+v", tail)
	}
}

func TestAPIDeletionDoesNotStopExistingCgroupReads(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "known-container")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "cpu.stat"), []byte("usage_usec 6000000\n"), 0644); err != nil {
		t.Fatal(err)
	}
	c, err := NewCollector(root, "n", 2*time.Second, 3*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	from := now.Add(-4 * time.Second)
	last := now.Add(-2 * time.Second)
	container := &containerState{row: Container{Name: "app", ID: "containerd://" + strings.Repeat("a", 64), StartedAt: from, Windows: []Window{{From: from, To: last, CPUSeconds: 2}}}, previous: &Counters{At: last, Usage: 4000000}, path: path}
	c.pods["uid-a"] = &podState{row: Pod{Name: "p", Namespace: "a", UID: "uid-a"}, containers: map[string]*containerState{"app/old": container}, lastSeen: now.Add(-time.Second)}
	c.Collect(nil, now)
	row := c.Snapshot().Pods[0].Containers[0]
	if row.FinishedAt != nil || row.EndUpperBound != nil || len(row.Windows) != 2 || row.Windows[1].CPUSeconds != 2 {
		t.Fatalf("API deletion dropped running work %+v", row)
	}
	// Losing just cpu.stat is not proof that the directory/process disappeared.
	if err := os.Remove(filepath.Join(path, "cpu.stat")); err != nil {
		t.Fatal(err)
	}
	c.Collect(nil, time.Now())
	row = c.Snapshot().Pods[0].Containers[0]
	if row.EndUpperBound != nil || row.FinishedAt != nil {
		t.Fatal("counter read failure was treated as termination")
	}
}
