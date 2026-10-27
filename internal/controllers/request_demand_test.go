package controllers

import (
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	requestforecast "github.com/cslab-ntua/HPA-Plus/internal/prediction/requestdemand"
	"github.com/cslab-ntua/HPA-Plus/internal/workloaddemand"
	"testing"
	"time"
)

func TestLearnedDemandUsesCurrentIntervalAndFutureBackgroundCapacity(t *testing.T) {
	now := time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)
	e := workloaddemand.Estimate{State: "qualified", CPUSecondsPerRequest: .02, BackgroundCPUPerPod: .1, FreshUntil: now.Add(time.Minute)}
	points := []requestforecast.Point{{At: now, Until: now.Add(30 * time.Second), Value: 120}, {At: now.Add(30 * time.Second), Until: now.Add(time.Minute), Value: 10}}
	n, _, err := requestReplicas(points, e, api.PredictivePolicy{}, 1, .6, 6, now)
	if err != nil || n != 5 {
		t.Fatalf("2.4/(0.6-0.1) must request5, got %d %v", n, err)
	}
	n, _, err = requestReplicas(points, e, api.PredictivePolicy{}, 1, .6, 5, now.Add(30*time.Second))
	if err != nil || n != 1 {
		t.Fatalf("future downscale did not wait correctly: %d %v", n, err)
	}
	e.BackgroundCPUPerPod = .6
	if _, _, err = requestReplicas(points, e, api.PredictivePolicy{}, 1, .6, 5, now); err == nil {
		t.Fatal("invalid per-pod capacity accepted")
	}
}

func TestNewFutureForecastPreservesOnlyIssuedCurrentInterval(t *testing.T) {
	now := time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)
	old := []requestforecast.Point{{At: now, Until: now.Add(30 * time.Second), Value: 120}, {At: now.Add(30 * time.Second), Until: now.Add(time.Minute), Value: 150}}
	fresh := []requestforecast.Point{{At: now.Add(30 * time.Second), Until: now.Add(time.Minute), Value: 10}}
	merged := mergeRequestInterval(old, fresh, now.Add(5*time.Second))
	if len(merged) != 2 || merged[0].Value != 120 || merged[1].Value != 10 {
		t.Fatalf("bad interval replacement: %+v", merged)
	}
	merged = mergeRequestInterval(old, fresh, now.Add(30*time.Second))
	if merged[0].Value != 10 {
		t.Fatalf("already-expired old interval survived: %+v", merged)
	}
}

func TestReplacingLaterForecastDoesNotDiscardInterveningIssuedInterval(t *testing.T) {
	now := time.Date(2026, 10, 20, 17, 0, 4, 0, time.UTC)
	old := []requestforecast.Point{{At: now.Truncate(30 * time.Second), Until: now.Truncate(30 * time.Second).Add(30 * time.Second), Value: 100}, {At: now.Truncate(30 * time.Second).Add(30 * time.Second), Until: now.Truncate(30 * time.Second).Add(time.Minute), Value: 200}}
	fresh := []requestforecast.Point{{At: now.Truncate(30 * time.Second).Add(time.Minute), Until: now.Truncate(30 * time.Second).Add(90 * time.Second), Value: 20}}
	merged := mergeRequestInterval(old, fresh, now)
	if len(merged) != 3 || merged[1].Value != 200 {
		t.Fatalf("issued intervening forecast disappeared: %+v", merged)
	}
	e := workloaddemand.Estimate{State: "qualified", CPUSecondsPerRequest: .02, FreshUntil: now.Add(2 * time.Minute)}
	if _, _, err := requestReplicas(fresh, e, api.PredictivePolicy{}, 1, .6, 4, now); err == nil {
		t.Fatal("uncovered interval claimed usable request forecast")
	}
}
