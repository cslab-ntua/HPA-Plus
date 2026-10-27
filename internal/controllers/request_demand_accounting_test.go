package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/cslab-ntua/HPA-Plus/internal/cgroup"
	"github.com/cslab-ntua/HPA-Plus/internal/workloaddemand"
	"k8s.io/apimachinery/pkg/labels"
)

type nativeAccountingGatherer struct {
	read func(string, time.Time, time.Time) (*cgroup.WorkloadObservation, error)
}

func (g nativeAccountingGatherer) GatherCgroupResources(context.Context, string, labels.Selector, time.Time, time.Time) (*cgroup.WorkloadObservation, error) {
	return nil, fmt.Errorf("unexpected fleet read")
}
func (g nativeAccountingGatherer) GatherCgroupPodResources(_ context.Context, _ string, _ string, uid string, from, to time.Time) (*cgroup.WorkloadObservation, error) {
	return g.read(uid, from, to)
}

func TestCostLearningUsesCompleteNativePairsAcrossEveryActivePod(t *testing.T) {
	from := time.Unix(1000, 0)
	slot := from.Add(30 * time.Second)
	traffic := workloaddemand.TrafficWindow{Qualified: true, Coverage: 1, ArrivalCoverage: 1, ExpectedPodSeconds: 60}
	for _, uid := range []string{"new", "mature"} {
		traffic.PerPodIntervals = append(traffic.PerPodIntervals, workloaddemand.PodCounterInterval{Pod: uid, UID: uid, Start: from, End: from.Add(28 * time.Second), CompletedRequests: 280})
		for i := 0; i < 14; i++ {
			traffic.PerPodNativeIntervals = append(traffic.PerPodNativeIntervals, workloaddemand.PodCounterInterval{Pod: uid, UID: uid, Start: from.Add(time.Duration(i*2) * time.Second), End: from.Add(time.Duration(i*2+2) * time.Second), CompletedRequests: 20})
		}
	}
	for _, missingWholePod := range []bool{false, true} {
		t.Run(fmt.Sprint(missingWholePod), func(t *testing.T) {
			g := nativeAccountingGatherer{read: func(uid string, left, right time.Time) (*cgroup.WorkloadObservation, error) {
				if uid == "new" && (left.Equal(from) || missingWholePod) {
					return &cgroup.WorkloadObservation{Complete: false, Coverage: 0, MissingReasons: []string{"startup counter gap"}}, nil
				}
				seconds := right.Sub(left).Seconds()
				return &cgroup.WorkloadObservation{From: left, To: right, Complete: true, Coverage: 1, CPUSeconds: seconds, RunningPodSeconds: seconds, SourceAt: right.Add(time.Second), ReceivedAt: slot.Add(4 * time.Second), BoundaryAveraged: true}, nil
			}}
			evidence := collectDemandNativeAccounting(context.Background(), g, "owned", "identity", slot, traffic)
			if missingWholePod {
				if evidence.Window.Qualified || evidence.Window.Coverage >= 1 {
					t.Fatalf("mature healthy subset qualified: %+v", evidence)
				}
				return
			}
			w := evidence.Window
			if !w.Qualified || w.CPUSeconds != 54 || w.CompletedRequests != 540 || w.PodSeconds != 54 || math.Abs(evidence.NativeSupportFraction-.9) > 1e-10 {
				t.Fatalf("E/D/P included unknown startup support or dropped valid later pairs: %+v", evidence)
			}
			if len(evidence.Pods) != 2 || !evidence.Pods[0].From.Equal(from.Add(2*time.Second)) || evidence.Pods[0].CompletedRequests != 260 {
				t.Fatalf("per-pod matched support lost: %+v", evidence.Pods)
			}
			if len(evidence.Rejections) == 0 || !evidence.Rejections[0].From.Equal(from) || !evidence.Rejections[0].To.Equal(from.Add(2*time.Second)) {
				t.Fatal("startup gap was hidden")
			}
		})
	}
}

func TestDemandScopeRetainsExactAbsentLifetimesAndUnknownEpochs(t *testing.T) {
	from := time.Unix(1000, 0)
	ended := from.Add(-time.Second)
	window := &cgroup.WorkloadObservation{From: from, To: from.Add(30 * time.Second), Inventory: []cgroup.Pod{
		{Namespace: "owned", Name: "retired", UID: "old", Containers: []cgroup.Container{{ID: "old-id", StartedAt: from.Add(-time.Minute), FinishedAt: &ended}}},
		{Namespace: "owned", Name: "unknown", UID: "unknown", Containers: []cgroup.Container{{Name: "app"}}},
		{Namespace: "foreign", Name: "foreign", UID: "foreign", Containers: []cgroup.Container{{ID: "f", StartedAt: from}}},
	}}
	scope := demandWindowScope(workloaddemand.Scope{Namespace: "owned", Pods: []workloaddemand.PodIdentity{{Name: "new", UID: "new"}}}, window)
	if len(scope.Pods) != 2 || !scope.Lifetimes["old"].Known || !scope.Lifetimes["old"].Intervals[0].To.Equal(ended) || scope.Lifetimes["unknown"].Known {
		t.Fatalf("absence or unknown epoch misclassified: %+v", scope)
	}
	if _, ok := scope.Lifetimes["new"]; ok {
		t.Fatal("missing current UID invented a known lifetime")
	}
	if _, ok := scope.Lifetimes["foreign"]; ok {
		t.Fatal("namespace boundary crossed")
	}
}

func TestNativeAccountingReusesImmutableValidatedBatchWithoutAPIPairReads(t *testing.T) {
	from := time.Unix(1000, 0)
	slot := from.Add(30 * time.Second)
	received := slot.Add(4 * time.Second)
	pod := cgroup.Pod{Namespace: "owned", Name: "app", UID: "uid", Labels: map[string]string{"arm": "request"}, Containers: []cgroup.Container{{Name: "app", ID: "container", StartedAt: from.Add(-time.Minute)}}}
	for i := 0; i < 15; i++ {
		left := from.Add(time.Duration(i*2) * time.Second)
		pod.Containers[0].Windows = append(pod.Containers[0].Windows, cgroup.Window{From: left, To: left.Add(2 * time.Second), CPUSeconds: 2})
	}
	snapshots := []cgroup.Snapshot{{Version: cgroup.Protocol, Node: "node", CollectedAt: received, Pods: []cgroup.Pod{pod}}}
	before, _ := json.Marshal(snapshots)
	batch := &cgroup.WorkloadObservation{ReceivedAt: received, NativeSnapshots: snapshots, InventoryComplete: true}
	traffic := workloaddemand.TrafficWindow{Qualified: true, Coverage: 1, ExpectedPodSeconds: 30, PerPodIntervals: []workloaddemand.PodCounterInterval{{Pod: "app", UID: "uid", Start: from.Add(time.Second), End: slot.Add(-time.Second), CompletedRequests: 280}}}
	g := nativeAccountingGatherer{read: func(string, time.Time, time.Time) (*cgroup.WorkloadObservation, error) {
		t.Fatal("retained batch caused an extra API read")
		return nil, nil
	}}
	evidence := collectDemandNativeAccounting(context.Background(), g, "owned", "identity", slot, traffic, batch)
	if !evidence.Window.Qualified || evidence.Window.CPUSeconds != 28 || evidence.Window.CompletedRequests != 280 || evidence.Window.PodSeconds != 28 {
		t.Fatalf("retained native batch mismatch: %+v", evidence)
	}
	after, _ := json.Marshal(snapshots)
	if string(before) != string(after) {
		t.Fatal("local per-pod view mutated retained snapshots")
	}
}
