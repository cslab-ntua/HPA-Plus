package controllers

import (
	"testing"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func startedPod(created time.Time, startup time.Duration) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.Time{Time: created}},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue,
			LastTransitionTime: metav1.Time{Time: created.Add(startup)}}}}}
}

func horizonModel(fromStartup bool) *api.Model {
	return &api.Model{Type: api.TypeXGBoost, XGBoost: &api.XGBoost{LookAhead: 90000, Lags: 12, HistorySize: 60},
		PredictivePolicy: &api.PredictivePolicy{ScaleUpLeadSeconds: 30, ScaleUpLeadFromStartup: fromStartup},
		RequestDemand:    &api.RequestDemandConfig{}}
}

func TestForecastHorizonCoversTheMeasuredStartup(t *testing.T) {
	window, configured := 30*time.Second, 90*time.Second
	created := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	events := []demandForecastEvent{{ConsumedAt: created, AvailableAt: created.Add(14 * time.Second)},
		{ConsumedAt: created.Add(30 * time.Second), AvailableAt: created.Add(37 * time.Second)}}
	// The standard runs: pods ready in 3 s, lead 8 s, forecasts 14 s late: ⌈22/30⌉ + 2 = 3 windows, today's 90 s.
	fast := []corev1.Pod{startedPod(created, 2*time.Second), startedPod(created, 3*time.Second)}
	if got := forecastHorizon(configured, window, horizonModel(true), fast, events); got != 90*time.Second {
		t.Fatalf("fast pods must keep 90 s, got %v", got)
	}
	// The slow-start run: pods ready in 63 s, lead 68 s: ⌈82/30⌉ + 2 = 5 windows.
	slow := []corev1.Pod{startedPod(created, 61*time.Second), startedPod(created, 63*time.Second)}
	if got := forecastHorizon(configured, window, horizonModel(true), slow, events); got != 150*time.Second {
		t.Fatalf("60 s pods need 150 s, got %v", got)
	}
	// Before any forecast the delay is taken as half a window: ⌈(68+15)/30⌉ + 2 = 5.
	if got := forecastHorizon(configured, window, horizonModel(true), slow, nil); got != 150*time.Second {
		t.Fatalf("no forecast yet: want 150 s, got %v", got)
	}
	// Without a measured lead the configured horizon stays.
	if got := forecastHorizon(configured, window, horizonModel(false), slow, events); got != configured {
		t.Fatalf("configured lead keeps the configured horizon, got %v", got)
	}
	if got := forecastHorizon(configured, window, horizonModel(true), nil, events); got != configured {
		t.Fatalf("no observed start-up keeps the configured horizon, got %v", got)
	}
	// A longer configured horizon is never shortened.
	if got := forecastHorizon(300*time.Second, window, horizonModel(true), slow, events); got != 300*time.Second {
		t.Fatalf("a longer configured horizon must stay, got %v", got)
	}
}

func TestForecastDelayIgnoresMissingAndImplausibleEvents(t *testing.T) {
	at := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	events := []demandForecastEvent{{ConsumedAt: at, AvailableAt: at.Add(9 * time.Second)}, {ConsumedAt: at},
		{ConsumedAt: at, AvailableAt: at.Add(-time.Second)}, {ConsumedAt: at, AvailableAt: at.Add(time.Hour)}}
	if got := forecastDelay(events, 30*time.Second); got != 9*time.Second {
		t.Fatalf("want 9 s, got %v", got)
	}
	if got := forecastDelay(nil, 30*time.Second); got != 15*time.Second {
		t.Fatalf("want half a window, got %v", got)
	}
}

func TestWithLookAheadCopiesTheModel(t *testing.T) {
	model := horizonModel(true)
	longer := withLookAhead(model, 150*time.Second)
	if longer.XGBoost.LookAhead != 150000 || model.XGBoost.LookAhead != 90000 {
		t.Fatalf("lookAhead must change on the copy only: %d %d", longer.XGBoost.LookAhead, model.XGBoost.LookAhead)
	}
	holt := withLookAhead(&api.Model{Type: api.TypeHoltWinters, HoltWinters: &api.HoltWinters{}}, 120*time.Second)
	if holt.HoltWinters.LookAhead == nil || *holt.HoltWinters.LookAhead != 120000 {
		t.Fatal("Holt-Winters lookAhead must be set")
	}
	toto := withLookAhead(&api.Model{Type: api.TypeToto, Toto: &api.Toto{LookAhead: 90000}}, 150*time.Second)
	if toto.Toto.LookAhead != 150000 {
		t.Fatal("Toto lookAhead must be set")
	}
}
