package controllers

import (
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	forecast "github.com/cslab-ntua/HPA-Plus/internal/prediction/requestdemand"
	demand "github.com/cslab-ntua/HPA-Plus/internal/workloaddemand"
	"testing"
	"time"
)

func TestFineRequestPublicationKeepsCurrentIntervalAndDecisionBoundary(t *testing.T) {
	base := time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)
	state := demandState{Forecasts: []forecast.Point{{At: base.Add(10 * time.Second), Until: base.Add(20 * time.Second), Value: 70}}, ForecastConsumedAt: base.Add(-30 * time.Second)}
	result := forecast.Result{TrainedThrough: base, Forecasts: []forecast.Point{{At: base.Add(10 * time.Second), Until: base.Add(20 * time.Second), Value: 5}, {At: base.Add(20 * time.Second), Until: base.Add(30 * time.Second), Value: 10}, {At: base.Add(30 * time.Second), Until: base.Add(40 * time.Second), Value: 10}}}
	publishRequestForecast(&state, result, base.Add(4*time.Second), base.Add(12*time.Second))
	if len(state.Forecasts) != 3 || state.Forecasts[0].Value != 70 || state.ForecastEvents[0].DroppedIntervals != 1 {
		t.Fatalf("late fine fit replaced the current forecast: %+v", state)
	}
	estimate := demand.Estimate{State: "qualified", CPUSecondsPerRequest: .02, FreshUntil: base.Add(time.Minute)}
	got, deadline, err := requestReplicas(state.Forecasts, estimate, api.PredictivePolicy{}, 1, .6, 3, base.Add(12*time.Second))
	if err != nil || got != 3 || !deadline.Equal(base.Add(20*time.Second)) {
		t.Fatalf("current interval discarded or wrong decision time: %d %v %v", got, deadline, err)
	}
	got, _, err = requestReplicas(state.Forecasts, estimate, api.PredictivePolicy{}, 1, .6, 3, base.Add(20*time.Second))
	if err != nil || got != 1 {
		t.Fatalf("downscale did not wait until measured applicability boundary: %d %v", got, err)
	}
}
