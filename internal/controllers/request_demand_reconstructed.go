package controllers

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
	"github.com/cslab-ntua/HPA-Plus/internal/hook"
	metricadapter "github.com/cslab-ntua/HPA-Plus/internal/metrics"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	requestforecast "github.com/cslab-ntua/HPA-Plus/internal/prediction/requestdemand"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A floor is computed only from a CPU window this recent. Windows close every
// thirty seconds; their counts are published a few seconds later.
const reconstructedFloorValidity = 60 * time.Second

// latestWindowClosed reports whether a sampled window ending at end is the
// newest one that can have closed by now. Windows close at every sampling
// period boundary and are stamped within a second or two of it, so a window
// ending well before the last boundary has a successor that is not stored yet:
// it describes demand from before that boundary.
func latestWindowClosed(end, now time.Time, period time.Duration) bool {
	return period <= 0 || !end.Before(now.Truncate(period).Add(-period/3))
}

// A sampled CPU window and a request window describe the same thirty seconds
// when their ends are this close. Sampled windows end at the collector's
// measurement time, request windows at the slot boundary.
const reconstructedWindowMatch = 15 * time.Second

func reconstructsCPU(model *api.Model) bool {
	return model.RequestDemand != nil && model.RequestDemand.ForecastTarget == api.RequestDemandTargetReconstructedCPU
}

func observedFeature(row requestforecast.Observation, name string) (float64, bool) {
	f, found := row.Features[name]
	if !found || f.Value == nil || math.IsNaN(*f.Value) || math.IsInf(*f.Value, 0) || *f.Value < 0 {
		return 0, false
	}
	return *f.Value, true
}

// backlogRatio is arrivals per completed unit of work in one closed window:
// about one when pods keep up, above one when completions fall behind. When
// pods started or stopped inside the window, its rates lack full support, but
// the ratio of both counters over the seconds they share is still measured.
func backlogRatio(row requestforecast.Observation) (float64, bool) {
	arrivals, haveArrivals := observedFeature(row, "traffic_arrivals")
	completions, haveCompletions := observedFeature(row, "traffic_completions")
	if haveArrivals && haveCompletions && completions > 0 {
		return arrivals / completions, true
	}
	return observedFeature(row, "traffic_backlog_ratio")
}

// windowRatio finds the request window ending closest to a sampled CPU window.
func windowRatio(rows []requestforecast.Observation, end time.Time) (float64, bool) {
	best, found := reconstructedWindowMatch+1, false
	ratio := 0.
	for _, row := range rows {
		gap := row.Until.Sub(end)
		if gap < 0 {
			gap = -gap
		}
		if gap > reconstructedWindowMatch || gap >= best {
			continue
		}
		if value, ok := backlogRatio(row); ok {
			best, ratio, found = gap, value, true
		}
	}
	return ratio, found
}

// reconstructSample rebuilds the CPU a sampled window needed: measured CPU
// scaled by that window's backlog ratio. CPU and completions describe the same
// window, so a change in request cost is included without a learned cost.
// Without a ratio the window keeps its measured CPU.
func reconstructSample(sample api.TimestampedSample, rows []requestforecast.Observation) (api.TimestampedSample, bool) {
	ratio, ok := windowRatio(rows, sample.Time.Time)
	if !ok {
		return sample, false
	}
	out := *sample.DeepCopy()
	millicores := int64(math.Round(float64(*sample.TotalCPUUsageMillicores) * ratio))
	out.TotalCPUUsageMillicores = &millicores
	if sample.Metric != nil {
		utilization := *sample.Metric * ratio
		out.Metric = &utilization
	}
	return out, true
}

// reconstructedHistory rebuilds the model's own sampled CPU history, keeping
// its samples, timestamps and capacity, and returns the newest size samples
// with how many of them kept measured CPU for lack of request counts.
func reconstructedHistory(sampled []api.TimestampedSample, rows []requestforecast.Observation, size int) ([]api.TimestampedSample, int) {
	cpu := prediction.FilterCPUHistory(sampled)
	if size > 0 && len(cpu) > size {
		cpu = cpu[len(cpu)-size:]
	}
	history := make([]api.TimestampedSample, 0, len(cpu))
	fallbacks := 0
	for _, sample := range cpu {
		if sample.Time == nil {
			continue
		}
		rebuilt, ok := reconstructSample(sample, rows)
		if !ok {
			fallbacks++
		}
		history = append(history, rebuilt)
	}
	return history, fallbacks
}

type reconstructedWindow struct {
	End                     time.Time
	Measured, Reconstructed float64
	HasRatio                bool
	// Previous is the reconstructed demand of the window before, when one
	// closed within reconstructedRisingLookback; it tells whether demand rises.
	Previous *float64
	// Fast is the reconstructed demand over the last few seconds, used only to
	// add capacity: a step shows in full within a fast window long before the
	// floor window, while removing capacity waits for the steadier floor.
	Fast *float64
}

// A window older than this cannot show whether demand is rising now.
const reconstructedRisingLookback = 2 * time.Minute

// Thirty-second windows that end at least this far apart barely overlap. A
// retried sample can end only seconds after the previous one and then covers
// mostly the same time, so it says nothing about a trend.
const reconstructedWindowSpacing = 25 * time.Second

// currentReconstructedWindow is the newest sampled CPU window if it is recent enough for a floor.
func currentReconstructedWindow(sampled []api.TimestampedSample, rows []requestforecast.Observation, now time.Time) *reconstructedWindow {
	cpu := prediction.FilterCPUHistory(sampled)
	if len(cpu) == 0 {
		return nil
	}
	latest := cpu[len(cpu)-1]
	if latest.Time == nil || now.Before(latest.Time.Time) || !now.Before(latest.Time.Add(reconstructedFloorValidity)) {
		return nil
	}
	rebuilt, ok := reconstructSample(latest, rows)
	window := &reconstructedWindow{End: latest.Time.Time, Measured: float64(*latest.TotalCPUUsageMillicores) / 1000,
		Reconstructed: float64(*rebuilt.TotalCPUUsageMillicores) / 1000, HasRatio: ok}
	window.Previous = previousReconstructed(cpu[:len(cpu)-1], rows, window.End)
	return window
}

// previousReconstructed is the reconstructed demand of the newest sampled
// window that ended at least reconstructedWindowSpacing before end and within
// reconstructedRisingLookback of it.
func previousReconstructed(cpu []api.TimestampedSample, rows []requestforecast.Observation, end time.Time) *float64 {
	for i := len(cpu) - 1; i >= 0; i-- {
		at := cpu[i].Time
		if at == nil || at.Time.After(end.Add(-reconstructedWindowSpacing)) {
			continue
		}
		if at.Time.Before(end.Add(-reconstructedRisingLookback)) {
			break
		}
		before, _ := reconstructSample(cpu[i], rows)
		previous := float64(*before.TotalCPUUsageMillicores) / 1000
		return &previous
	}
	return nil
}

// The floor measures demand over the most recent half sampling period at
// decision time, so it reflects a change within seconds instead of after the
// next sampled window closes.
func reconstructedFloorWindow(period time.Duration) time.Duration {
	window := period / 2
	if window < 2*time.Second {
		window = 2 * time.Second
	}
	if window > time.Minute {
		window = time.Minute
	}
	return window
}

// The fast window is a third of the floor window, so a step shows in full
// within a few seconds; decisions run at least once per fast window.
func reconstructedFastWindow(period time.Duration) time.Duration {
	window := reconstructedFloorWindow(period) / 3
	if window < 2*time.Second {
		window = 2 * time.Second
	}
	return window
}

// latestRatio is the backlog ratio of the newest request window that ended
// shortly before end: request windows are published every sampling period, so
// a window measured now is scaled by the ratio that was current when it began.
func latestRatio(rows []requestforecast.Observation, end time.Time, period time.Duration) (float64, bool) {
	best := time.Time{}
	ratio, found := 0., false
	for _, row := range rows {
		if row.Until.After(end.Add(time.Second)) || row.Until.Before(end.Add(-period-reconstructedWindowMatch)) || !row.Until.After(best) {
			continue
		}
		if value, ok := backlogRatio(row); ok {
			best, ratio, found = row.Until, value, true
		}
	}
	return ratio, found
}

// recentReconstructedWindow measures current demand for the floor at decision
// time. It is nil when the collectors cannot measure every pod; the floor then
// uses the newest sampled window.
func (r *HPAPlusReconciler) recentReconstructedWindow(ctx context.Context, instance *api.HPAPlus, scale *autoscalingv1.Scale,
	rows []requestforecast.Observation, sampled []api.TimestampedSample) *reconstructedWindow {
	provider, ok := r.Gatherer.(metricadapter.RecentCPUGatherer)
	if !ok {
		return nil
	}
	period := syncPeriodForInstance(instance)
	snapshot, err := r.collectRecentCPU(ctx, instance, scale, provider, reconstructedFloorWindow(period))
	if err != nil || snapshot.MetricsIncomplete || snapshot.TotalUsageMillicores == nil || snapshot.SourceTime.IsZero() {
		return nil
	}
	measured := float64(*snapshot.TotalUsageMillicores) / 1000
	window := &reconstructedWindow{End: snapshot.SourceTime, Measured: measured, Reconstructed: measured}
	ratio, hasRatio := latestRatio(rows, window.End, period)
	if hasRatio {
		window.Reconstructed, window.HasRatio = measured*ratio, true
	}
	window.Previous = previousReconstructed(prediction.FilterCPUHistory(sampled), rows, window.End)
	fast, err := r.collectRecentCPU(ctx, instance, scale, provider, reconstructedFastWindow(period))
	if err == nil && !fast.MetricsIncomplete && fast.TotalUsageMillicores != nil && !fast.SourceTime.IsZero() {
		demand := float64(*fast.TotalUsageMillicores) / 1000
		if hasRatio {
			demand *= ratio
		}
		window.Fast = &demand
	}
	return window
}

// Demand must grow by at least this many pods' worth of CPU between windows
// to count as rising. Smaller changes are window-to-window noise at low load.
const reconstructedMinimumRise = 0.25

// demandRising reports whether the newer window needs more pods than the one
// before it, by more than the scale-up tolerance and the minimum rise. Pods
// are compared unrounded: a rise from 2.04 to 2.94 pods is real even though
// both round up to 3.
func demandRising(newer, older float64, capacity prediction.Capacity, tolerance float64) bool {
	perPod := float64(capacity.RequestPerPod) * float64(capacity.TargetPercentage) / 100 / 1000
	if perPod <= 0 {
		return false
	}
	before, need := older/perPod, newer/perPod
	return need-before >= reconstructedMinimumRise && need > before*(1+tolerance)
}

// runReconstructedForecast runs the model's ordinary CPU forecaster on the
// reconstructed history and publishes its trajectory in request-forecast form,
// with values in cores.
func (r *HPAPlusReconciler) runReconstructedForecast(ctx context.Context, instance *api.HPAPlus, model *api.Model,
	history []api.TimestampedSample, targetIdentity string, status *api.ModelStatus) (requestforecast.Result, error) {
	if len(history) == 0 {
		return requestforecast.Result{}, errors.New("no reconstructed history")
	}
	requested := r.now()
	status.LastAttemptTime = &metav1.Time{Time: requested}
	started := time.Now()
	scoped := *model
	// Keep worker diagnostics apart from the measured-CPU fallback forecast.
	scoped.SessionID = model.SessionID + "/reconstructed"
	latest := history[len(history)-1]
	// Capacity comes from the same snapshot the history was built with.
	scoped.CPURequestPerPodMillicores, scoped.TargetCPUUtilizationPercentage = *latest.RequestPerPodMillicores, *latest.TargetCPUUtilizationPercentage
	observation := prediction.CPUObservation(&scoped)
	observation.Target, observation.At, observation.Value = targetIdentity, latest.Time.Time, *latest.TotalCPUUsageMillicores
	observation.SourceID = "reconstructed/" + latest.Time.UTC().Format(time.RFC3339)
	result, err := r.modelRuntime().Predict(hook.WithResource(ctx, instance.Namespace, instance.Name),
		prediction.Request{Model: scoped, History: history, Observation: &observation})
	outcome := "success"
	switch {
	case errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled):
		outcome = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		outcome = "timeout"
	case errors.Is(err, prediction.ErrInsufficientHistory):
		outcome = "insufficient_history"
	case err != nil:
		outcome = "failed"
	}
	elapsed := time.Since(started)
	duration := elapsed.Milliseconds()
	status.LastRunDurationMilliseconds, status.LastRunOutcome = &duration, outcome
	r.Telemetry.ObserveModelRun(model.Type, outcome, elapsed)
	if err != nil {
		return requestforecast.Result{}, err
	}
	if result.ConsumedUntil == nil || len(result.Trajectory) == 0 {
		return requestforecast.Result{}, errors.New("reconstructed forecast lacks a timed trajectory")
	}
	status.LastSuccessTime = &metav1.Time{Time: r.now()}
	out := requestforecast.Result{Status: "ready", Unit: "cores", GeneratedAt: r.now(), TrainedThrough: *result.ConsumedUntil}
	for _, p := range result.Trajectory {
		at, atErr := time.Parse(time.RFC3339Nano, p.At)
		until, untilErr := time.Parse(time.RFC3339Nano, p.Until)
		if atErr != nil || untilErr != nil {
			return requestforecast.Result{}, fmt.Errorf("invalid reconstructed trajectory time")
		}
		out.Forecasts = append(out.Forecasts, requestforecast.Point{At: at, Until: until, Value: float64(p.CPU) / 1000})
	}
	return out, nil
}

func cpuForecastPoints(points []requestforecast.Point) []api.CPUForecastPoint {
	out := make([]api.CPUForecastPoint, 0, len(points))
	for _, p := range points {
		out = append(out, api.CPUForecastPoint{At: p.At.UTC().Format(time.RFC3339Nano), Until: p.Until.UTC().Format(time.RFC3339Nano),
			CPU: int64(math.Round(p.Value * 1000))})
	}
	return out
}

type reconstructedDecision struct {
	Replicas, ForecastOnly, Floor int32
	DecidedBy                     string
	Deadline                      time.Time
}

// reconstructedReplicas converts the forecast exactly as CPU forecasts are
// converted, then never recommends fewer replicas than the latest closed
// window's reconstructed demand needs. The forecast can still add capacity
// early; the floor only stops a forecast that is wrong right now from removing
// capacity the current traffic needs. Until the window that closed at the last
// sampling boundary is stored, or while demand is still rising, it holds
// current capacity.
// splitRisingWindows places a forecast rise where it most likely happens. Each
// forecast is the average of its window, so a step inside a window shows as a
// value between its neighbours, and the share of the window after the step is
// (value-before)/(after-before). From the estimated step on, the window takes
// the next window's level, so the scale-up lead counts from the step itself
// rather than from the next window's start. A step on a window boundary leaves
// the share at zero and changes nothing; decreases are never moved earlier.
func splitRisingWindows(points []api.CPUForecastPoint, capacity prediction.Capacity, tolerance float64) []api.CPUForecastPoint {
	out := make([]api.CPUForecastPoint, 0, len(points)+2)
	for k, p := range points {
		if k == 0 || k == len(points)-1 {
			out = append(out, p)
			continue
		}
		before, value, after := float64(points[k-1].CPU)/1000, float64(p.CPU)/1000, float64(points[k+1].CPU)/1000
		share := 0.
		if after > before && demandRising(after, value, capacity, tolerance) {
			share = math.Min(1, math.Max(0, (value-before)/(after-before)))
		}
		at, e1 := time.Parse(time.RFC3339Nano, p.At)
		until, e2 := time.Parse(time.RFC3339Nano, p.Until)
		if e1 != nil || e2 != nil || share == 0 {
			out = append(out, p)
			continue
		}
		step := until.Add(-time.Duration(share * float64(until.Sub(at)))).UTC().Format(time.RFC3339Nano)
		if step != p.At {
			out = append(out, api.CPUForecastPoint{At: p.At, Until: step, CPU: p.CPU})
		}
		out = append(out, api.CPUForecastPoint{At: step, Until: p.Until, CPU: points[k+1].CPU})
	}
	if len(out) > algorithm.MaxTrajectoryPoints+1 {
		return points
	}
	return out
}

// windowDemand is the demand a forecast sets now: its window in progress, or a
// window starting within the scale-up lead. covered reports a window in progress.
func windowDemand(points []api.CPUForecastPoint, lead time.Duration, now time.Time) (demand float64, covered bool) {
	for _, p := range points {
		at, e1 := time.Parse(time.RFC3339Nano, p.At)
		until, e2 := time.Parse(time.RFC3339Nano, p.Until)
		if e1 != nil || e2 != nil {
			continue
		}
		inProgress := !now.Before(at) && now.Before(until)
		covered = covered || inProgress
		if (inProgress || (at.After(now) && !at.After(now.Add(lead)))) && float64(p.CPU)/1000 > demand {
			demand = float64(p.CPU) / 1000
		}
	}
	return demand, covered
}

// postponedRiseHold keeps the pods the previous forecast asked for when the
// newest one takes back a rise the previous expected by now. Forecast timing
// is uncertain by tens of seconds, and a forecast made after a quiet window
// that should have been busy tends to push the rise away, releasing pods just
// before it arrives. The hold applies only while the previous forecast's
// demand for now rises over the demand measured when it was made and over the
// demand measured now; it never exceeds the current replicas and ends with the
// window that was in progress when the newest forecast arrived.
func postponedRiseHold(events []demandForecastEvent, history []api.TimestampedSample, latest *reconstructedWindow,
	policy api.PredictivePolicy, capacity prediction.Capacity, current int32, now time.Time) (int32, time.Time, bool) {
	if len(events) < 2 || latest == nil || current <= 0 {
		return 0, time.Time{}, false
	}
	newest, previous := events[len(events)-1], events[len(events)-2]
	if newest.AvailableAt.IsZero() || previous.ConsumedAt.IsZero() || !previous.AvailableAt.Before(newest.AvailableAt) || now.Before(newest.AvailableAt) {
		return 0, time.Time{}, false
	}
	windows := splitRisingWindows(prediction.ForecastWindows(cpuForecastPoints(previous.Forecasts)), capacity, policy.EffectiveTolerance())
	var until time.Time
	for _, p := range windows {
		at, e1 := time.Parse(time.RFC3339Nano, p.At)
		end, e2 := time.Parse(time.RFC3339Nano, p.Until)
		if e1 == nil && e2 == nil && !newest.AvailableAt.Before(at) && newest.AvailableAt.Before(end) {
			until = end
		}
	}
	if until.IsZero() || !now.Before(until) {
		return 0, time.Time{}, false
	}
	expected, covered := windowDemand(windows, time.Duration(policy.ScaleUpLeadSeconds)*time.Second, now)
	// The demand measured when the previous forecast was made: its last window.
	then, found := 0., false
	for _, s := range history {
		if s.Time != nil && s.TotalCPUUsageMillicores != nil && !s.Time.Time.After(previous.ConsumedAt) {
			then, found = float64(*s.TotalCPUUsageMillicores)/1000, true
		}
	}
	tolerance := policy.EffectiveTolerance()
	if !covered || !found || !demandRising(expected, then, capacity, tolerance) || !demandRising(expected, latest.Reconstructed, capacity, tolerance) {
		return 0, time.Time{}, false
	}
	result, err := prediction.ResolveDemand(prediction.DemandResult{Demand: prediction.Demand{Kind: prediction.CPUKind(), Value: int64(math.Round(expected * 1000))}},
		prediction.Observation{Kind: prediction.CPUKind(), Capacity: capacity})
	if err != nil {
		return 0, time.Time{}, false
	}
	if result.Replicas > current {
		return current, until, true
	}
	return result.Replicas, until, true
}

func reconstructedReplicas(points []requestforecast.Point, latest *reconstructedWindow, policy api.PredictivePolicy,
	capacity prediction.Capacity, current int32, now time.Time, period time.Duration) (reconstructedDecision, error) {
	// Each forecast applies to the window it predicts, not one step later, and
	// a rise inside a window counts from where it most likely starts.
	windows := splitRisingWindows(prediction.ForecastWindows(cpuForecastPoints(points)), capacity, policy.EffectiveTolerance())
	forecast, deadline, err := prediction.TimedWindowReplicas(windows, policy, capacity, current, now)
	if err != nil {
		return reconstructedDecision{}, err
	}
	decision := reconstructedDecision{Replicas: forecast, ForecastOnly: forecast, DecidedBy: "forecast", Deadline: deadline}
	if latest == nil || now.Before(latest.End) || !now.Before(latest.End.Add(reconstructedFloorValidity)) ||
		!latestWindowClosed(latest.End, now, period) {
		// Current demand is unmeasured, typically while pods start or stop, or
		// just after a boundary before its window is stored: keep current
		// capacity rather than let the forecast alone scale in.
		decision.Floor = current
		if current > forecast {
			decision.Replicas, decision.DecidedBy = current, "hold"
		}
		return decision, nil
	}
	// The floor window is already wall-clock time, not a predicted sample.
	window := []requestforecast.Point{{At: latest.End, Until: latest.End.Add(reconstructedFloorValidity), Value: latest.Reconstructed}}
	floor, floorDeadline, err := prediction.TimedWindowReplicas(cpuForecastPoints(window), policy, capacity, current, now)
	if err != nil {
		return reconstructedDecision{}, err
	}
	decision.Floor = floor
	if !floorDeadline.IsZero() && (decision.Deadline.IsZero() || floorDeadline.Before(decision.Deadline)) {
		decision.Deadline = floorDeadline
	}
	if floor > forecast {
		decision.Replicas, decision.DecidedBy = floor, "floor"
	}
	// While demand is still rising the last window understates it, so neither
	// the forecast nor the floor may remove capacity yet.
	if latest.Previous != nil && demandRising(latest.Reconstructed, *latest.Previous, capacity, policy.EffectiveTolerance()) && current > decision.Replicas {
		decision.Replicas, decision.DecidedBy = current, "rising"
	}
	// A step or burst shows in the fast window first. It may only add capacity.
	if latest.Fast != nil {
		fast := []requestforecast.Point{{At: latest.End, Until: latest.End.Add(reconstructedFloorValidity), Value: *latest.Fast}}
		replicas, _, err := prediction.TimedWindowReplicas(cpuForecastPoints(fast), policy, capacity, current, now)
		if err != nil {
			return reconstructedDecision{}, err
		}
		if replicas > current && replicas > decision.Replicas {
			decision.Replicas, decision.DecidedBy = replicas, "fast"
		}
	}
	return decision, nil
}

// reconstructedRecommendation reports the latest window and returns the
// published forecast's recommendation with its floor. An unavailable path
// fails like the arrivals path, so the controller falls back to the measured
// CPU forecast and then holds.
func (r *HPAPlusReconciler) reconstructedRecommendation(model *api.Model, scale *autoscalingv1.Scale, snapshot cpuMetricSnapshot,
	state demandState, sampled, history []api.TimestampedSample, fallbacks, size int, period time.Duration, recent *reconstructedWindow,
	status *api.ModelStatus, fail func(string) (int32, time.Time, bool)) (int32, time.Time, bool) {
	diagnostic := status.RequestDemand
	diagnostic.ReconstructionFallbacks = fallbacks
	latest := recent
	if latest == nil {
		latest = currentReconstructedWindow(sampled, state.Observations, r.now())
	}
	if latest != nil {
		diagnostic.MeasuredCores, diagnostic.ReconstructedCores = latest.Measured, latest.Reconstructed
	}
	if snapshot.RequestPerPodMillicores == nil || snapshot.TargetUtilizationPercentage == nil {
		return fail("CPU capacity configuration unavailable")
	}
	if len(history) < size {
		return fail("reconstructed demand history warming up")
	}
	if model.PredictionMaxAge == nil || model.PredictivePolicy == nil || state.ForecastConsumedAt.IsZero() ||
		!r.now().Before(state.ForecastConsumedAt.Add(time.Duration(*model.PredictionMaxAge)*time.Millisecond)) {
		return fail("reconstructed forecast missing or expired")
	}
	capacity := prediction.Capacity{Kind: prediction.CPUKind(), Policy: "cpu-request-utilization",
		RequestPerPod: *snapshot.RequestPerPodMillicores, TargetPercentage: *snapshot.TargetUtilizationPercentage}
	decision, err := reconstructedReplicas(state.Forecasts, latest, *model.PredictivePolicy, capacity, scale.Spec.Replicas, r.now(), period)
	if err != nil {
		return fail("reconstructed forecast interval unavailable")
	}
	if held, until, ok := postponedRiseHold(state.ForecastEvents, history, latest, *model.PredictivePolicy, capacity, scale.Spec.Replicas, r.now()); ok && held > decision.Replicas {
		decision.Replicas, decision.DecidedBy = held, "postponed"
		if decision.Deadline.IsZero() || until.Before(decision.Deadline) {
			decision.Deadline = until
		}
	}
	ageDeadline := state.ForecastConsumedAt.Add(time.Duration(*model.PredictionMaxAge) * time.Millisecond)
	deadline := decision.Deadline
	if deadline.IsZero() || ageDeadline.Before(deadline) {
		deadline = ageDeadline
	}
	diagnostic.State, diagnostic.FreshUntil = "qualified", &metav1.Time{Time: ageDeadline}
	diagnostic.Source, diagnostic.FallbackReason = "request-demand", ""
	diagnostic.ForecastOnlyReplicas, diagnostic.FloorReplicas, diagnostic.DecidedBy = decision.ForecastOnly, decision.Floor, decision.DecidedBy
	status.State = "Ready"
	status.HistorySamples = int64(len(history))
	replicas := decision.Replicas
	status.ForecastReplicas = &replicas
	status.ConsumedUntil = &metav1.Time{Time: state.ForecastConsumedAt}
	status.ExpiresAt = &metav1.Time{Time: deadline}
	return replicas, deadline, true
}
