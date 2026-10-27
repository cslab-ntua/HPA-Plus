package controllers

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	requestforecast "github.com/cslab-ntua/HPA-Plus/internal/prediction/requestdemand"
	"github.com/cslab-ntua/HPA-Plus/internal/workloaddemand"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func demandRow(until time.Time, values map[string]float64) requestforecast.Observation {
	row := requestforecast.Observation{At: until.Add(-30 * time.Second), Until: until, Features: map[string]requestforecast.FeatureValue{}}
	for name, value := range values {
		v := value
		row.Features[name] = requestforecast.FeatureValue{Value: &v, SourceAt: until}
	}
	return row
}

func cpuSample(end time.Time, millicores int64, replicas int32) api.TimestampedSample {
	request, target := int64(1000), int32(60)
	utilization := float64(millicores) * 100 / float64(int64(replicas)*request)
	return api.TimestampedSample{Time: &metav1.Time{Time: end}, Replicas: replicas, Metric: &utilization,
		TotalCPUUsageMillicores: &millicores, RequestPerPodMillicores: &request, TargetCPUUtilizationPercentage: &target}
}

func TestReconstructSampleScalesMeasuredCPUByBacklog(t *testing.T) {
	slot := time.Date(2026, 10, 21, 5, 0, 30, 0, time.UTC)
	// Sampled CPU windows end at the collector time, about a second before the slot.
	sample := cpuSample(slot.Add(-time.Second), 2000, 2)
	for _, c := range []struct {
		name          string
		rows          []requestforecast.Observation
		millicores    int64
		reconstructed bool
	}{
		{"keeping up", []requestforecast.Observation{demandRow(slot, map[string]float64{"traffic_arrivals": 50, "traffic_completions": 50})}, 2000, true},
		// r10 cycles s=330: two saturated pods completed 29.6 of 76.6 req/s.
		{"saturated", []requestforecast.Observation{demandRow(slot, map[string]float64{"traffic_arrivals": 76.6, "traffic_completions": 29.6})}, 5176, true},
		{"missing completions", []requestforecast.Observation{demandRow(slot, map[string]float64{"traffic_arrivals": 76.6})}, 2000, false},
		// A pod started inside the window: rates lack full support, the ratio over shared seconds remains.
		{"ratio over shared seconds", []requestforecast.Observation{demandRow(slot, map[string]float64{"traffic_backlog_ratio": 1.25})}, 2500, true},
		{"full-window rates first", []requestforecast.Observation{demandRow(slot, map[string]float64{"traffic_arrivals": 50, "traffic_completions": 40, "traffic_backlog_ratio": 2})}, 2500, true},
		{"no completed work", []requestforecast.Observation{demandRow(slot, map[string]float64{"traffic_arrivals": 3, "traffic_completions": 0})}, 2000, false},
		{"no window for this sample", []requestforecast.Observation{demandRow(slot.Add(-30*time.Second), map[string]float64{"traffic_arrivals": 76.6, "traffic_completions": 29.6})}, 2000, false},
		{"closest window wins", []requestforecast.Observation{
			demandRow(slot.Add(-30*time.Second), map[string]float64{"traffic_arrivals": 9, "traffic_completions": 9}),
			demandRow(slot, map[string]float64{"traffic_arrivals": 76.6, "traffic_completions": 29.6})}, 5176, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, reconstructed := reconstructSample(sample, c.rows)
			if reconstructed != c.reconstructed || *got.TotalCPUUsageMillicores != c.millicores || !got.Time.Time.Equal(sample.Time.Time) {
				t.Fatalf("got %d %v, want %d %v", *got.TotalCPUUsageMillicores, reconstructed, c.millicores, c.reconstructed)
			}
			if want := float64(c.millicores) / 20; math.Abs(*got.Metric-want) > .05 {
				t.Fatalf("utilization not rebuilt with demand: %v want %v", *got.Metric, want)
			}
		})
	}
	if *sample.TotalCPUUsageMillicores != 2000 {
		t.Fatal("reconstruction modified the sampled history")
	}
}

func TestReconstructedHistoryRebuildsNewestSampledWindows(t *testing.T) {
	slot := time.Date(2026, 10, 21, 5, 0, 0, 0, time.UTC)
	var sampled []api.TimestampedSample
	var rows []requestforecast.Observation
	for i := 0; i < 6; i++ {
		end := slot.Add(time.Duration(i-5) * 30 * time.Second)
		sampled = append(sampled, cpuSample(end.Add(-time.Second), 300, 1))
		if i != 3 {
			rows = append(rows, demandRow(end, map[string]float64{"traffic_arrivals": 9, "traffic_completions": 9}))
		}
	}
	sampled[5] = cpuSample(slot.Add(-time.Second), 2000, 2)
	rows[len(rows)-1] = demandRow(slot, map[string]float64{"traffic_arrivals": 76.6, "traffic_completions": 29.6})
	sampled = append([]api.TimestampedSample{{Time: &metav1.Time{Time: slot.Add(-time.Hour)}, Replicas: 1}}, sampled...)
	history, fallbacks := reconstructedHistory(sampled, rows, 4)
	if len(history) != 4 || fallbacks != 1 {
		t.Fatalf("want newest four CPU samples with one window lacking counts, got %d/%d", len(history), fallbacks)
	}
	if *history[3].TotalCPUUsageMillicores != 5176 || *history[1].TotalCPUUsageMillicores != 300 || !history[3].Time.Time.Equal(slot.Add(-time.Second)) {
		t.Fatalf("history not rebuilt per window: %d %d", *history[3].TotalCPUUsageMillicores, *history[1].TotalCPUUsageMillicores)
	}
}

func TestReconstructedReplicasFloorStopsWrongScaleIn(t *testing.T) {
	slot := time.Date(2026, 10, 21, 5, 0, 30, 0, time.UTC)
	now := slot.Add(5 * time.Second)
	tolerance := .1
	policy := api.PredictivePolicy{ScaleUpLeadSeconds: 30, Tolerance: &tolerance}
	capacity := prediction.Capacity{Kind: prediction.CPUKind(), Policy: "cpu-request-utilization", RequestPerPod: 1000, TargetPercentage: 60}
	forecast := func(cores float64) []requestforecast.Point {
		return []requestforecast.Point{{At: slot, Until: slot.Add(30 * time.Second), Value: cores}, {At: slot.Add(30 * time.Second), Until: slot.Add(time.Minute), Value: cores}}
	}
	window := func(cores float64) *reconstructedWindow {
		return &reconstructedWindow{End: slot.Add(-time.Second), Reconstructed: cores}
	}
	got, err := reconstructedReplicas(forecast(1.2), window(2*76.6/29.6), policy, capacity, 3, now, 30*time.Second)
	if err != nil || got.Replicas != 9 || got.ForecastOnly != 2 || got.Floor != 9 || got.DecidedBy != "floor" {
		t.Fatalf("floor must replace a wrong scale-in: %+v %v", got, err)
	}
	got, err = reconstructedReplicas(forecast(2.9), window(.3), policy, capacity, 1, now, 30*time.Second)
	if err != nil || got.Replicas != 5 || got.Floor != 1 || got.DecidedBy != "forecast" {
		t.Fatalf("forecast must still scale up ahead of demand: %+v %v", got, err)
	}
	// Within the existing 10% tolerance of current capacity, the floor holds.
	got, err = reconstructedReplicas(forecast(.6), window(2.6), policy, capacity, 4, now, 30*time.Second)
	if err != nil || got.Floor != 4 || got.Replicas != 4 {
		t.Fatalf("floor ignored rounding tolerance: %+v %v", got, err)
	}
	// Without a recent CPU window the floor holds current capacity, so a
	// wrong forecast cannot scale in while demand is unmeasured.
	got, err = reconstructedReplicas(forecast(1.2), nil, policy, capacity, 3, now, 30*time.Second)
	if err != nil || got.Floor != 3 || got.Replicas != 3 || got.ForecastOnly != 2 || got.DecidedBy != "hold" {
		t.Fatalf("unmeasured demand must hold current capacity: %+v %v", got, err)
	}
	got, err = reconstructedReplicas(forecast(4.2), nil, policy, capacity, 3, now, 30*time.Second)
	if err != nil || got.Replicas != 7 || got.DecidedBy != "forecast" {
		t.Fatalf("hold must not block a forecast scale-up: %+v %v", got, err)
	}
	sampled := []api.TimestampedSample{cpuSample(slot.Add(-time.Second), 2000, 2)}
	if currentReconstructedWindow(sampled, nil, slot.Add(-time.Second).Add(reconstructedFloorValidity)) != nil {
		t.Fatal("stale CPU window must not set a floor")
	}
	if w := currentReconstructedWindow(sampled, nil, now); w == nil || w.Reconstructed != 2 || w.HasRatio {
		t.Fatalf("window without counts must floor at measured CPU: %+v", w)
	}
}

func TestReconstructedTargetRequiresThirtySecondGrid(t *testing.T) {
	config := api.RequestDemandConfig{Traffic: api.TrafficProviderConfig{Provider: "prometheus", ArrivalsMetric: "started_total", CompletionsMetric: "finished_total",
		ArrivalSemantics: "started", CompletionSemantics: "finished", RetrySemantics: "attempts"}, ForecastTarget: api.RequestDemandTargetReconstructedCPU}
	if err := workloaddemand.ValidateConfig(config); err != nil {
		t.Fatal(err)
	}
	config.ForecastIntervalSeconds = 10
	if workloaddemand.ValidateConfig(config) == nil {
		t.Fatal("reconstructed target accepted a sub-window forecast grid")
	}
	config.ForecastIntervalSeconds, config.ForecastTarget = 0, "requests"
	if workloaddemand.ValidateConfig(config) == nil {
		t.Fatal("unknown forecast target accepted")
	}
}

type reconstructedRuntime struct {
	requests []prediction.Request
	cpu      int64
}

func (f *reconstructedRuntime) Predict(_ context.Context, request prediction.Request) (prediction.Result, error) {
	f.requests = append(f.requests, request)
	last := request.History[len(request.History)-1].Time.Time
	result := prediction.Result{ConsumedUntil: &last}
	for i := 1; i <= 3; i++ {
		at := last.Add(time.Duration(i) * 30 * time.Second)
		result.Trajectory = append(result.Trajectory, api.CPUForecastPoint{At: at.Format(time.RFC3339Nano), Until: at.Add(30 * time.Second).Format(time.RFC3339Nano), CPU: f.cpu})
	}
	return result, nil
}
func (*reconstructedRuntime) Prune(_ *api.Model, rows []api.TimestampedSample) ([]api.TimestampedSample, error) {
	return rows, nil
}
func (*reconstructedRuntime) SyncSessions(string, map[string]bool) {}

func TestReconstructedTargetForecastsRebuiltDemandAndFloorsDecision(t *testing.T) {
	r, owner, scale, now := demandIntegrationFixture(t)
	r.TrafficPrometheusURL = "http://unused.invalid"
	ctx := context.Background()
	model := &owner.Spec.Models[0]
	model.RequestDemand.ForecastTarget = api.RequestDemandTargetReconstructedCPU
	tolerance := .1
	model.PredictivePolicy = &api.PredictivePolicy{ScaleUpLeadSeconds: 30, Tolerance: &tolerance}
	object := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "a", Name: "app"}, object); err != nil {
		t.Fatal(err)
	}
	raw, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	template := raw["spec"].(map[string]interface{})["template"]
	identity := demandHash([]interface{}{string(owner.UID), scaleTargetIdentity(owner, scale), model, template, demandMeasurementSource, r.TrafficPrometheusURL})
	cm, state, err := r.loadDemandState(ctx, owner, model, identity)
	if err != nil {
		t.Fatal(err)
	}
	// The cost estimator never qualified: the reconstructed path must not need it.
	state.CreatedAt = now.Add(-time.Hour)
	state.LastSlot = now.Add(-4 * time.Second)
	state.LastFitSlot = state.LastSlot.Add(-30 * time.Second)
	var sampled []api.TimestampedSample
	for i := 0; i < 62; i++ {
		until := state.LastSlot.Add(time.Duration(i-61) * 30 * time.Second)
		values := map[string]float64{"traffic_arrivals": 9, "traffic_completions": 9}
		millicores := int64(300)
		if i >= 59 {
			values, millicores = map[string]float64{"traffic_arrivals": 76.6, "traffic_completions": 29.6}, 2000
		}
		if i == 30 {
			delete(values, "traffic_completions")
		}
		// Request windows routinely lack whole-window CPU during pod churn; the
		// sampled CPU history supplies it.
		state.Observations = append(state.Observations, demandRow(until, values))
		sampled = append(sampled, cpuSample(until.Add(-time.Second), millicores, 3))
	}
	if err := r.saveDemandState(ctx, cm, state); err != nil {
		t.Fatal(err)
	}
	r.WorkloadRunner = demandIntegrationRunner(func(string, string, int) (string, error) {
		t.Fatal("reconstructed target must not run the arrival worker")
		return "", nil
	})
	fake := &reconstructedRuntime{cpu: 1200}
	r.Runtime = fake
	request, target := int64(1000), int32(60)
	snapshot := cpuMetricSnapshot{RequestPerPodMillicores: &request, TargetUtilizationPercentage: &target, TargetIdentity: scaleTargetIdentity(owner, scale)}
	report := &api.ModelStatus{}
	// Without the current window's CPU sample the slot is not fitted yet.
	if _, _, usable := r.processRequestDemand(ctx, owner, model, scale, snapshot, &api.ModelStatus{}, sampled[:61]); usable || len(fake.requests) != 0 {
		t.Fatal("fitted before the current window's CPU sample existed")
	}
	n, _, usable := r.processRequestDemand(ctx, owner, model, scale, snapshot, report, sampled)
	if !usable || n != 8+1 {
		t.Fatalf("saturated current demand must floor a low forecast: n=%d usable=%v status=%+v", n, usable, report.RequestDemand)
	}
	d := report.RequestDemand
	if d.Source != "request-demand" || d.State != "qualified" || d.Target != api.RequestDemandTargetReconstructedCPU || d.DecidedBy != "floor" ||
		d.ForecastOnlyReplicas != 2 || d.FloorReplicas != 9 || d.MeasuredCores != 2 || math.Abs(d.ReconstructedCores-2*76.6/29.6) > 1e-3 ||
		d.ReconstructionFallbacks != 1 || report.HistorySamples != 60 {
		t.Fatalf("reconstructed status incomplete: %+v samples=%d", d, report.HistorySamples)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("CPU forecaster not run exactly once: %d", len(fake.requests))
	}
	history := fake.requests[0].History
	if len(history) != 60 || *history[59].TotalCPUUsageMillicores != 5176 || *history[0].TotalCPUUsageMillicores != 300 {
		t.Fatalf("CPU forecaster did not receive reconstructed demand: %d %v", len(history), *history[59].TotalCPUUsageMillicores)
	}
	if fake.requests[0].Model.SessionID != model.SessionID+"/reconstructed" {
		t.Fatal("reconstructed forecast shares the measured-CPU forecast session")
	}
	// The published events are what the benchmark supervisor checks for eligibility.
	updated := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(cm), updated); err != nil {
		t.Fatal(err)
	}
	var saved demandState
	if err := json.Unmarshal([]byte(updated.Data["state"]), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.ForecastEvents) != 1 || len(saved.Forecasts) != 3 || saved.Forecasts[0].Value != 1.2 || !saved.ForecastConsumedAt.Equal(sampled[61].Time.Time) ||
		saved.Forecasts[0].Until.Sub(saved.Forecasts[0].At) != 30*time.Second || !saved.LastFitSlot.Equal(state.LastSlot) {
		t.Fatalf("reconstructed forecast not published: %+v", saved.ForecastEvents)
	}
	// The next reconciliation reuses the published forecast instead of refitting.
	*now = now.Add(10 * time.Second)
	report = &api.ModelStatus{}
	if n, _, usable = r.processRequestDemand(ctx, owner, model, scale, snapshot, report, sampled); !usable || n != 9 || len(fake.requests) != 1 {
		t.Fatalf("published forecast not reused: n=%d usable=%v fits=%d", n, usable, len(fake.requests))
	}
}

func TestRisingDemandBlocksScaleIn(t *testing.T) {
	slot := time.Date(2026, 10, 21, 13, 46, 30, 0, time.UTC)
	now := slot.Add(4 * time.Second)
	tolerance := .1
	policy := api.PredictivePolicy{ScaleUpLeadSeconds: 30, Tolerance: &tolerance}
	capacity := prediction.Capacity{Kind: prediction.CPUKind(), Policy: "cpu-request-utilization", RequestPerPod: 1000, TargetPercentage: 60}
	forecast := []requestforecast.Point{{At: slot, Until: slot.Add(30 * time.Second), Value: 1.2}, {At: slot.Add(30 * time.Second), Until: slot.Add(time.Minute), Value: 1.2}}
	window := func(newer float64, older *float64) *reconstructedWindow {
		return &reconstructedWindow{End: slot.Add(-time.Second), Reconstructed: newer, Previous: older}
	}
	ptr := func(v float64) *float64 { return &v }
	// r13 cycle 1: 0.27 -> 1.35 cores needs 1 -> 3 pods, so 5 pods stay.
	got, err := reconstructedReplicas(forecast, window(1.35, ptr(.27)), policy, capacity, 5, now, 30*time.Second)
	if err != nil || got.Replicas != 5 || got.DecidedBy != "rising" {
		t.Fatalf("a ramp up must block scale-in: %+v %v", got, err)
	}
	for _, c := range []struct {
		name          string
		newer, older  float64
		current, want int32
	}{
		{"flat low load", .33, .30, 3, 2},
		// r14 19:28-19:30: quiet windows vary by up to a fifth of a pod.
		{"noise at low load", .37, .29, 3, 2},
		{"flat high load", 4.09, 3.84, 9, 7},
		{"within the rounding tolerance", 4.25, 4.2, 9, 8},
		{"falling", 1.35, 2.4, 5, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := reconstructedReplicas(forecast, window(c.newer, ptr(c.older)), policy, capacity, c.current, now, 30*time.Second)
			if err != nil || got.Replicas != c.want || got.DecidedBy == "rising" {
				t.Fatalf("no rise, normal scale-in expected: %+v %v", got, err)
			}
		})
	}
	if got, _ := reconstructedReplicas(forecast, window(1.35, nil), policy, capacity, 5, now, 30*time.Second); got.DecidedBy == "rising" {
		t.Fatal("without an earlier window a rise cannot be shown")
	}
	// Rises that rounding to whole pods hid in r14.
	for _, c := range []struct {
		name         string
		newer, older float64
		current      int32
	}{
		{"19:22:59, 2.04 to 2.94 pods", 1.766, 1.224, 5},
		{"19:31:36, 0.60 to 1.03 pods", .617, .36, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := reconstructedReplicas(forecast, window(c.newer, ptr(c.older)), policy, capacity, c.current, now, 30*time.Second)
			if err != nil || got.Replicas != c.current || got.DecidedBy != "rising" {
				t.Fatalf("a rise within one rounded pod must block scale-in: %+v %v", got, err)
			}
		})
	}
}

func TestFloorWaitsForTheWindowThatJustClosed(t *testing.T) {
	// r14 peak 3: at 19:12:30 the window ending 19:12:29 was not stored yet,
	// so the floor read the quiet window before it and let 5 pods drop to 3.
	slot := time.Date(2026, 10, 21, 16, 12, 30, 0, time.UTC)
	tolerance := .1
	policy := api.PredictivePolicy{ScaleUpLeadSeconds: 30, Tolerance: &tolerance}
	capacity := prediction.Capacity{Kind: prediction.CPUKind(), Policy: "cpu-request-utilization", RequestPerPod: 1000, TargetPercentage: 60}
	forecast := func(cores float64) []requestforecast.Point {
		return []requestforecast.Point{{At: slot, Until: slot.Add(30 * time.Second), Value: cores}, {At: slot.Add(30 * time.Second), Until: slot.Add(time.Minute), Value: cores}}
	}
	quiet := &reconstructedWindow{End: slot.Add(-31 * time.Second), Reconstructed: .37}
	busy := &reconstructedWindow{End: slot.Add(-time.Second), Reconstructed: 2.55, Previous: &quiet.Reconstructed}
	got, err := reconstructedReplicas(forecast(1.5), quiet, policy, capacity, 5, slot.Add(500*time.Millisecond), 30*time.Second)
	if err != nil || got.Replicas != 5 || got.ForecastOnly != 3 || got.DecidedBy != "hold" {
		t.Fatalf("a window from before the last boundary must not set a floor: %+v %v", got, err)
	}
	got, err = reconstructedReplicas(forecast(4.2), quiet, policy, capacity, 5, slot.Add(500*time.Millisecond), 30*time.Second)
	if err != nil || got.Replicas != 7 || got.DecidedBy != "forecast" {
		t.Fatalf("waiting for the window must not block a forecast scale-up: %+v %v", got, err)
	}
	got, err = reconstructedReplicas(forecast(1.5), busy, policy, capacity, 5, slot.Add(4*time.Second), 30*time.Second)
	if err != nil || got.Replicas != 5 || got.Floor != 5 || got.DecidedBy != "floor" {
		t.Fatalf("once stored, the busy window must floor the forecast: %+v %v", got, err)
	}
	// A window stamped just before or after its boundary is the latest one.
	for _, end := range []time.Time{slot.Add(-time.Second), slot, slot.Add(time.Second)} {
		if !latestWindowClosed(end, slot.Add(29*time.Second), 30*time.Second) {
			t.Fatalf("window ending %v treated as stale", end)
		}
	}
	if latestWindowClosed(slot.Add(-time.Second), slot.Add(30*time.Second), 30*time.Second) {
		t.Fatal("a window from the boundary before treated as the latest")
	}
}

func TestCurrentReconstructedWindowFindsThePreviousWindow(t *testing.T) {
	slot := time.Date(2026, 10, 21, 13, 46, 30, 0, time.UTC)
	sampled := []api.TimestampedSample{
		cpuSample(slot.Add(-181*time.Second), 900, 1), // beyond the lookback
		cpuSample(slot.Add(-91*time.Second), 270, 1),  // r13 16:44:29: the last window before the ramp
		cpuSample(slot.Add(-25*time.Second), 1310, 5), // r13 16:45:35: a retry overlapping the newest by 24 s
		cpuSample(slot.Add(-time.Second), 1790, 5),    // r13 16:45:59
	}
	w := currentReconstructedWindow(sampled, nil, slot.Add(4*time.Second))
	if w == nil || w.Previous == nil || *w.Previous != .27 || w.Reconstructed != 1.79 {
		t.Fatalf("previous non-overlapping window not found: %+v", w)
	}
	w = currentReconstructedWindow(sampled[:1:1], nil, slot.Add(-178*time.Second))
	if w == nil || w.Previous != nil {
		t.Fatalf("a lone window has no previous: %+v", w)
	}
	w = currentReconstructedWindow([]api.TimestampedSample{sampled[0], sampled[3]}, nil, slot.Add(4*time.Second))
	if w == nil || w.Previous != nil {
		t.Fatalf("a window older than the lookback cannot show a rise: %+v", w)
	}
}

func TestLatestRatioUsesTheNewestPublishedWindow(t *testing.T) {
	end := time.Date(2026, 10, 21, 19, 17, 14, 0, time.UTC)
	rows := []requestforecast.Observation{
		demandRow(end.Add(-74*time.Second), map[string]float64{"traffic_arrivals": 9, "traffic_completions": 9}),
		demandRow(end.Add(-44*time.Second), map[string]float64{"traffic_arrivals": 12, "traffic_completions": 12}),
		demandRow(end.Add(-14*time.Second), map[string]float64{"traffic_arrivals": 66, "traffic_completions": 24.4}),
		demandRow(end.Add(16*time.Second), map[string]float64{"traffic_arrivals": 1, "traffic_completions": 2}),
	}
	ratio, ok := latestRatio(rows, end, 30*time.Second)
	if !ok || math.Abs(ratio-66/24.4) > 1e-9 {
		t.Fatalf("newest closed request window not used: %v %v", ratio, ok)
	}
	if _, ok := latestRatio(rows[:1], end, 30*time.Second); ok {
		t.Fatal("a request window older than one period and the match tolerance was used")
	}
}

func TestDecisionTimeWindowSetsTheFloorBetweenBoundaries(t *testing.T) {
	// A window measured at decision time ends seconds ago, mid-slot: it is the
	// latest measurement and must set the floor rather than hold.
	slot := time.Date(2026, 10, 21, 16, 12, 30, 0, time.UTC)
	now := slot.Add(15 * time.Second)
	tolerance := .1
	policy := api.PredictivePolicy{ScaleUpLeadSeconds: 30, Tolerance: &tolerance}
	capacity := prediction.Capacity{Kind: prediction.CPUKind(), Policy: "cpu-request-utilization", RequestPerPod: 1000, TargetPercentage: 60}
	forecast := []requestforecast.Point{{At: slot, Until: slot.Add(30 * time.Second), Value: 1.5}, {At: slot.Add(30 * time.Second), Until: slot.Add(time.Minute), Value: 1.5}}
	quiet := .37
	recent := &reconstructedWindow{End: now.Add(-time.Second), Measured: 2.55, Reconstructed: 2.55, Previous: &quiet}
	got, err := reconstructedReplicas(forecast, recent, policy, capacity, 5, now, 30*time.Second)
	if err != nil || got.Replicas != 5 || got.Floor != 5 || got.DecidedBy != "floor" {
		t.Fatalf("decision-time window must floor a mid-peak forecast drop: %+v %v", got, err)
	}
	if reconstructedFloorWindow(30*time.Second) != 15*time.Second || reconstructedFloorWindow(time.Second) != 2*time.Second {
		t.Fatal("floor window is not half the sampling period within bounds")
	}
}

func TestFastWindowOnlyAddsCapacity(t *testing.T) {
	// g1 05:16:05: the forecast held 3 pods for a 2.8-core peak; the 15 s
	// floor window still averaged mostly quiet seconds when the peak began.
	slot := time.Date(2026, 10, 22, 2, 16, 0, 0, time.UTC)
	now := slot.Add(10 * time.Second)
	tolerance := .1
	policy := api.PredictivePolicy{ScaleUpLeadSeconds: 8, Tolerance: &tolerance}
	capacity := prediction.Capacity{Kind: prediction.CPUKind(), Policy: "cpu-request-utilization", RequestPerPod: 1000, TargetPercentage: 60}
	forecast := []requestforecast.Point{{At: slot, Until: slot.Add(30 * time.Second), Value: 1.6}, {At: slot.Add(30 * time.Second), Until: slot.Add(time.Minute), Value: 1.6}}
	ptr := func(v float64) *float64 { return &v }
	window := &reconstructedWindow{End: now.Add(-time.Second), Measured: 1.1, Reconstructed: 1.1, Fast: ptr(2.8)}
	got, err := reconstructedReplicas(forecast, window, policy, capacity, 3, now, 30*time.Second)
	if err != nil || got.Replicas != 5 || got.DecidedBy != "fast" {
		t.Fatalf("a step in the fast window must add capacity at once: %+v %v", got, err)
	}
	// Within the scale-up tolerance of current capacity the fast window adds nothing.
	window.Fast = ptr(1.95)
	if got, _ = reconstructedReplicas(forecast, window, policy, capacity, 3, now, 30*time.Second); got.Replicas != 3 || got.DecidedBy == "fast" {
		t.Fatalf("noise within the tolerance must not scale up: %+v", got)
	}
	// A quiet fast window never removes what the floor or the forecast keep.
	window.Reconstructed, window.Fast = 2.5, ptr(.3)
	if got, _ = reconstructedReplicas(forecast, window, policy, capacity, 5, now, 30*time.Second); got.Replicas != 5 || got.DecidedBy == "fast" {
		t.Fatalf("the fast window must never remove capacity: %+v", got)
	}
	if reconstructedFastWindow(30*time.Second) != 5*time.Second || reconstructedFastWindow(time.Second) != 2*time.Second {
		t.Fatal("fast window is not a third of the floor window within bounds")
	}
}

func TestPostponedRiseKeepsPodsUntilTheWindowEnds(t *testing.T) {
	// g2 with r18, 08:31: one forecast expected the peak from 08:31:00 and the
	// candidate went to 4 pods; the window stayed quiet, the next forecast
	// postponed the rise and the pods were released 16 s before it arrived.
	slot := time.Date(2026, 10, 23, 5, 31, 0, 0, time.UTC)
	tolerance := .1
	policy := api.PredictivePolicy{ScaleUpLeadSeconds: 8, Tolerance: &tolerance}
	capacity := prediction.Capacity{Kind: prediction.CPUKind(), Policy: "cpu-request-utilization", RequestPerPod: 1000, TargetPercentage: 60}
	// Published points carry their window's end; ForecastWindows moves them back.
	forecast := func(cores ...float64) []requestforecast.Point {
		out := []requestforecast.Point{}
		for i, v := range cores {
			end := slot.Add(time.Duration(i+1) * 30 * time.Second)
			out = append(out, requestforecast.Point{At: end, Until: end.Add(30 * time.Second), Value: v})
		}
		return out
	}
	events := []demandForecastEvent{
		{IssuedAt: slot.Add(4 * time.Second), AvailableAt: slot.Add(8 * time.Second), ConsumedAt: slot, Forecasts: forecast(2.21, 2.28, 2.35)},
		{IssuedAt: slot.Add(34 * time.Second), AvailableAt: slot.Add(37 * time.Second), ConsumedAt: slot.Add(30 * time.Second), Forecasts: forecast(1.16, 1.16, 1.18)[1:]},
	}
	sample := func(at time.Time, millicores int64) api.TimestampedSample {
		return api.TimestampedSample{Time: &metav1.Time{Time: at}, Replicas: 2, TotalCPUUsageMillicores: &millicores}
	}
	history := []api.TimestampedSample{sample(slot, 760), sample(slot.Add(30*time.Second), 880)}
	quiet := &reconstructedWindow{End: slot.Add(35 * time.Second), Measured: .88, Reconstructed: .88}
	held, until, ok := postponedRiseHold(events, history, quiet, policy, capacity, 4, slot.Add(40*time.Second))
	if !ok || held != 4 || !until.Equal(slot.Add(time.Minute)) {
		t.Fatalf("a postponed rise must keep its pods to the end of the window: %d %v %v", held, until, ok)
	}
	if _, _, ok = postponedRiseHold(events, history, quiet, policy, capacity, 4, slot.Add(61*time.Second)); ok {
		t.Fatal("the hold must end with the window that was in progress")
	}
	if held, _, _ = postponedRiseHold(events, history, quiet, policy, capacity, 3, slot.Add(40*time.Second)); held != 3 {
		t.Fatalf("the hold must never add pods: %d", held)
	}
	busy := &reconstructedWindow{End: slot.Add(35 * time.Second), Measured: 2.3, Reconstructed: 2.3}
	if _, _, ok = postponedRiseHold(events, history, busy, policy, capacity, 4, slot.Add(40*time.Second)); ok {
		t.Fatal("a rise that has arrived is the floor's, not a postponed one")
	}
	duringPeak := []api.TimestampedSample{sample(slot, 2500), sample(slot.Add(30*time.Second), 880)}
	if _, _, ok = postponedRiseHold(events, duringPeak, quiet, policy, capacity, 4, slot.Add(40*time.Second)); ok {
		t.Fatal("a forecast made during a peak that still expects it is not a postponed rise")
	}
}

func TestForecastRiseCountsFromTheStepInsideItsWindow(t *testing.T) {
	// g3 17:18:45: the peak began half-way through the window 17:18:30-17:19:00,
	// whose forecast was its average, between the quiet window before it and the
	// peak after it. The peak's pods followed the next window and came 7 s late.
	slot := time.Date(2026, 10, 22, 14, 18, 30, 0, time.UTC)
	tolerance := .1
	policy := api.PredictivePolicy{ScaleUpLeadSeconds: 8, Tolerance: &tolerance}
	capacity := prediction.Capacity{Kind: prediction.CPUKind(), Policy: "cpu-request-utilization", RequestPerPod: 1000, TargetPercentage: 60}
	// Published points carry their window's end; ForecastWindows moves them back.
	forecast := func(cores ...float64) []requestforecast.Point {
		out := []requestforecast.Point{}
		for i, v := range cores {
			end := slot.Add(time.Duration(i) * 30 * time.Second)
			out = append(out, requestforecast.Point{At: end, Until: end.Add(30 * time.Second), Value: v})
		}
		return out
	}
	decide := func(points []requestforecast.Point, after time.Duration) int32 {
		got, err := reconstructedReplicas(points, nil, policy, capacity, 3, slot.Add(after), 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return got.Replicas
	}
	// Half-way between 0.6 and 3.3 cores: the step is 15 s into the window, and
	// the peak's 6 pods are due the lead before it.
	inside := forecast(.6, 1.95, 3.3)
	if decide(inside, 6*time.Second) != 3 || decide(inside, 8*time.Second) != 6 {
		t.Fatalf("a rise inside a window must count from its estimated step: %d at +6 s, %d at +8 s",
			decide(inside, 6*time.Second), decide(inside, 8*time.Second))
	}
	// A step on the boundary keeps the next window's own timing.
	boundary := forecast(.6, .6, 3.3)
	if decide(boundary, 8*time.Second) != 3 || decide(boundary, 22*time.Second) != 6 {
		t.Fatal("a step on a window boundary must not be moved earlier")
	}
	windows := prediction.ForecastWindows(cpuForecastPoints(inside))
	split := splitRisingWindows(windows, capacity, tolerance)
	if len(split) != 4 || split[1].Until != slot.Add(15*time.Second).Format(time.RFC3339Nano) || split[2].CPU != 3300 {
		t.Fatalf("the window must split at its estimated step: %+v", split)
	}
	// Decreases and rises within the tolerance keep the forecast as published.
	for _, cores := range [][]float64{{3.3, 1.95, .6}, {.6, .65, .7}} {
		windows = prediction.ForecastWindows(cpuForecastPoints(forecast(cores...)))
		if got := splitRisingWindows(windows, capacity, tolerance); !reflect.DeepEqual(got, windows) {
			t.Fatalf("%v must not be split: %+v", cores, got)
		}
	}
}
