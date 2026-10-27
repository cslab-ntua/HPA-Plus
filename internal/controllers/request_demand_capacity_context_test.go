package controllers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	requestforecast "github.com/cslab-ntua/HPA-Plus/internal/prediction/requestdemand"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCapacityContextHistoryRetainsCausalReadAcrossLateReconciles(t *testing.T) {
	boundary := time.Date(2026, 10, 20, 23, 1, 0, 0, time.UTC)
	request := int64(1000)
	state := demandState{}
	recordDemandCapacityContext(&state, demandCapacityContext{ObservedAt: boundary.Add(-2 * time.Second), Requested: 4, Ready: 3, Running: 4, CPURequestMillicores: &request})
	recordDemandCapacityContext(&state, demandCapacityContext{ObservedAt: boundary.Add(200 * time.Millisecond), Requested: 8, Ready: 4, Running: 5, CPURequestMillicores: &request})
	row := requestforecast.Observation{Until: boundary, ReceivedAt: boundary.Add(31 * time.Second)}
	selectDemandCapacityFeatures(&row, state.CapacityHistory)
	if len(row.Features) != 4 || *row.Features["requested_replicas"].Value != 4 || *row.Features["ready_replicas"].Value != 3 || *row.Features["cpu_request_per_pod"].Value != 1 {
		t.Fatalf("lost causal capacity read: %+v", row.Features)
	}
	for _, v := range row.Features {
		if !v.SourceAt.Equal(boundary.Add(-2*time.Second)) || !v.ReceivedAt.Equal(v.SourceAt) {
			t.Fatal("capacity measurement was backdated")
		}
	}
	next := requestforecast.Observation{Until: boundary.Add(30 * time.Second)}
	selectDemandCapacityFeatures(&next, state.CapacityHistory)
	if *next.Features["requested_replicas"].Value != 8 || !next.Features["requested_replicas"].SourceAt.Equal(boundary.Add(200*time.Millisecond)) {
		t.Fatal("later snapshot never became available to subsequent interval")
	}
	empty := requestforecast.Observation{Until: boundary.Add(-3 * time.Second)}
	selectDemandCapacityFeatures(&empty, state.CapacityHistory)
	if len(empty.Features) != 0 {
		t.Fatal("future capacity was substituted for missing historical data")
	}
}

func TestCapacityContextHistoryPreservesUnitsMissingValuesAndRestart(t *testing.T) {
	origin := time.Date(2026, 10, 20, 23, 1, 0, 0, time.UTC)
	q := int64(250)
	state := demandState{}
	recordDemandCapacityContext(&state, demandCapacityContext{ObservedAt: origin, Requested: 1, Ready: 1, Running: 1, CPURequestMillicores: &q})
	q = 900 // Input pointers must not mutate retained historical readings.
	row := requestforecast.Observation{Until: origin}
	selectDemandCapacityFeatures(&row, state.CapacityHistory)
	if *row.Features["cpu_request_per_pod"].Value != .25 {
		t.Fatal("CPU request unit or retained pointer changed")
	}
	recordDemandCapacityContext(&state, demandCapacityContext{ObservedAt: origin.Add(30 * time.Second), Requested: 2})
	row = requestforecast.Observation{Until: origin.Add(30 * time.Second)}
	selectDemandCapacityFeatures(&row, state.CapacityHistory)
	if row.Features["cpu_request_per_pod"].Value != nil || state.Context["cpu_request_per_pod"].Value != nil {
		t.Fatal("unavailable request resurrected an older value")
	}
	for i := 2; i < 30; i++ {
		recordDemandCapacityContext(&state, demandCapacityContext{ObservedAt: origin.Add(time.Duration(i) * 30 * time.Second), Requested: int32(i)})
	}
	if len(state.CapacityHistory) != 16 {
		t.Fatal("capacity context history is not bounded")
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var restarted demandState
	if err = json.Unmarshal(raw, &restarted); err != nil {
		t.Fatal(err)
	}
	if len(restarted.CapacityHistory) != 16 || restarted.CapacityHistory[15].Requested != 29 {
		t.Fatal("restart lost timestamped capacity context")
	}
}

func TestCapacityContextSurvivesRealControllerAcquisitionsAfterPhaseDrift(t *testing.T) {
	r, owner, scale, now := demandIntegrationFixture(t)
	boundary := now.Truncate(30 * time.Second)
	*now = boundary.Add(31 * time.Second)
	r.TrafficPrometheusURL = "http://unused.invalid"
	ctx := context.Background()
	model := &owner.Spec.Models[0]
	object := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "a", Name: "app"}, object); err != nil {
		t.Fatal(err)
	}
	raw, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	identity := demandHash([]interface{}{string(owner.UID), scaleTargetIdentity(owner, scale), model, raw["spec"].(map[string]interface{})["template"], demandMeasurementSource, r.TrafficPrometheusURL})
	cm, state, err := r.loadDemandState(ctx, owner, model, identity)
	if err != nil {
		t.Fatal(err)
	}
	state.CreatedAt = boundary.Add(-time.Hour)
	state.LastSlot = boundary.Add(-30 * time.Second)
	request, target := int64(1000), int32(60)
	recordDemandCapacityContext(&state, demandCapacityContext{ObservedAt: boundary.Add(-2 * time.Second), Requested: 4, Ready: 3, Running: 4, CPURequestMillicores: &request})
	recordDemandCapacityContext(&state, demandCapacityContext{ObservedAt: boundary.Add(200 * time.Millisecond), Requested: 8, Ready: 4, Running: 5, CPURequestMillicores: &request})
	if err = r.saveDemandState(ctx, cm, state); err != nil {
		t.Fatal(err)
	}
	for step := 0; step < 90; step++ {
		want := float64(3)
		if step == 0 {
			want = 4
		}
		if step == 1 {
			want = 8
		}
		if step > 0 {
			*now = now.Add(30 * time.Second)
		}
		r.processRequestDemand(ctx, owner, model, scale, cpuMetricSnapshot{RequestPerPodMillicores: &request, TargetUtilizationPercentage: &target}, &api.ModelStatus{}, nil)
		_, stored, err := r.loadDemandState(ctx, owner, model, identity)
		if err != nil {
			t.Fatal(err)
		}
		latest := stored.Observations[len(stored.Observations)-1]
		value, ok := latest.Features["requested_replicas"]
		if !ok || value.Value == nil || *value.Value != want || value.SourceAt.After(latest.Until) || value.ReceivedAt.After(latest.ReceivedAt) {
			t.Fatalf("step%d lost or backdated capacity: value=%+v row=%+v", step, value, latest)
		}
	}
}
