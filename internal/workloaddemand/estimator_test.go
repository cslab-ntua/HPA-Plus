package workloaddemand

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func sampleWindow(i int, c, b float64) Window {
	start := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC).Add(time.Duration(i) * 30 * time.Second)
	pods := float64(1 + i%3)
	completed := float64(80 + (i%7)*85)
	return Window{ID: start.Format(time.RFC3339), Start: start, End: start.Add(30 * time.Second), AvailableAt: start.Add(31 * time.Second), CPUSeconds: c*completed + b*pods*30, CompletedRequests: completed, PodSeconds: pods * 30, Coverage: 1, Qualified: true}
}
func trained(t *testing.T) *Estimator {
	t.Helper()
	e := NewEstimator("a")
	for i := 0; i < 25; i++ {
		w := sampleWindow(i, .02, .05)
		e.Observe(w, w.AvailableAt)
	}
	return e
}
func TestOnlineLearningBackgroundAndCapacity(t *testing.T) {
	e := trained(t)
	last := sampleWindow(24, .02, .05)
	got := e.Estimate(last.AvailableAt)
	if got.State != "qualified" || math.Abs(got.CPUSecondsPerRequest-.02) > 1e-10 || math.Abs(got.BackgroundCPUPerPod-.05) > 1e-10 {
		t.Fatalf("incorrect online fit: %+v", got)
	}
	ratio, err := RequiredReplicas(120, 1, .6, got, last.AvailableAt)
	if err != nil || math.Abs(ratio-2.4/.55) > 1e-9 {
		t.Fatalf("background capacity calculation: %v %v", ratio, err)
	}
	if _, err = RequiredReplicas(120, .05, .6, got, last.AvailableAt); err == nil {
		t.Fatal("nonpositive processing capacity must be unavailable")
	}
}
func TestConstantPerPodTrafficIsUnidentifiable(t *testing.T) {
	e := NewEstimator("constant")
	for i := 0; i < 25; i++ {
		w := sampleWindow(i, .02, .05)
		w.CompletedRequests = w.PodSeconds * 5
		w.CPUSeconds = .02*w.CompletedRequests + .05*w.PodSeconds
		e.Observe(w, w.AvailableAt)
	}
	if got := e.Estimate(sampleWindow(24, 0, 0).AvailableAt); got.State != "unidentifiable" {
		t.Fatalf("constant per-pod rate incorrectly qualified: %+v", got)
	}
}
func TestMissingFutureDuplicateAndOverlappingWindows(t *testing.T) {
	e := trained(t)
	before := e.Snapshot()
	w := sampleWindow(25, .02, .05)
	w.Coverage = .9
	e.Observe(w, w.AvailableAt)
	if len(e.Snapshot().Windows) != len(before.Windows) {
		t.Fatal("missing data was admitted")
	}
	w = sampleWindow(25, .02, .05)
	e.Observe(w, w.End.Add(-time.Second))
	if len(e.Snapshot().Windows) != len(before.Windows) {
		t.Fatal("future data was admitted")
	}
	w = sampleWindow(24, .02, .05)
	e.Observe(w, w.AvailableAt)
	if len(e.Snapshot().Windows) != len(before.Windows) {
		t.Fatal("duplicate was admitted")
	}
	w = sampleWindow(25, .02, .05)
	w.Start = w.Start.Add(-time.Second)
	e.Observe(w, w.AvailableAt)
	if len(e.Snapshot().Windows) != len(before.Windows) {
		t.Fatal("overlap was admitted")
	}
	if got := e.Estimate(before.Last.FreshUntil); got.State != "stale" {
		t.Fatalf("exact freshness boundary: %+v", got)
	}
}
func TestPreUpdateDriftAndRecovery(t *testing.T) {
	e := trained(t)
	for i := 25; i < 28; i++ {
		w := sampleWindow(i, .2, .05)
		got := e.Observe(w, w.AvailableAt)
		if i == 27 && got.State != "drifted" {
			t.Fatalf("three errors did not mark drift: %+v", got)
		}
	}
	for i := 28; i < 31; i++ {
		w := sampleWindow(i, 0, 0)
		prior := e.Estimate(w.Start)
		w.CPUSeconds = prior.CPUSecondsPerRequest*w.CompletedRequests + prior.BackgroundCPUPerPod*w.PodSeconds
		got := e.Observe(w, w.AvailableAt)
		if i < 30 && got.State != "drifted" {
			t.Fatal("resumed before three good checks")
		}
		if i == 30 && got.State != "qualified" {
			t.Fatalf("did not resume: %+v", got)
		}
	}
}
func TestRestartRetainsLearningAndDoesNotReuseAnotherIdentity(t *testing.T) {
	e := trained(t)
	data, err := json.Marshal(e.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err = json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	restored, err := Restore("a", state)
	if err != nil {
		t.Fatal(err)
	}
	w := sampleWindow(24, .02, .05)
	before := restored.Snapshot()
	restored.Observe(w, w.AvailableAt)
	if restored.Snapshot().Last.Version != before.Last.Version || len(restored.Snapshot().Windows) != len(before.Windows) {
		t.Fatal("restart replay changed estimator")
	}
	if _, err = Restore("new-uid", state); err == nil {
		t.Fatal("identity fence not enforced")
	}
}
