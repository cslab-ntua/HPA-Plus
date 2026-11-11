package controllers

import (
	"testing"
	"time"

	"github.com/cslab-ntua/HPA-Plus/internal/cgroup"
	"github.com/cslab-ntua/HPA-Plus/internal/workloaddemand"
)

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
