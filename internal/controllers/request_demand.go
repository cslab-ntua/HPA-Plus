package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/cgroup"
	requestforecast "github.com/cslab-ntua/HPA-Plus/internal/prediction/requestdemand"
	"github.com/cslab-ntua/HPA-Plus/internal/workloaddemand"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	demandStateLimit        = 768 << 10
	demandPublicationDelay  = 4 * time.Second
	demandMeasurementSource = "cgroup-window-v3-capacity-context"
)

func hasRequestDemand(instance *api.HPAPlus) bool {
	for _, model := range instance.Spec.Models {
		if model.RequestDemand != nil {
			return true
		}
	}
	return false
}

// requestDemandRequeue adds acquisition deadlines without displacing earlier
// timed decisions or scale-write cooldown expiry. The publication clock is
// independent of reconciliation duration and forecast applicability boundaries.
func requestDemandRequeue(instance *api.HPAPlus, now time.Time, regular time.Duration, cooldownUntil time.Time) time.Duration {
	if !hasRequestDemand(instance) {
		return regular
	}
	nextPublication := now.Add(-demandPublicationDelay).UTC().Truncate(30 * time.Second).Add(30*time.Second + demandPublicationDelay)
	if delay := nextPublication.Sub(now); delay < regular {
		regular = delay
	}
	if delay := cooldownUntil.Sub(now); delay > 0 && delay < regular {
		regular = delay
	}
	// The reconstructed floor measures demand when a decision runs: decide at
	// least once per fast window so a step is acted on within seconds.
	for i := range instance.Spec.Models {
		if reconstructsCPU(&instance.Spec.Models[i]) {
			if limit := reconstructedFastWindow(syncPeriodForInstance(instance)); limit < regular {
				regular = limit
			}
			break
		}
	}
	return regular
}

// The source describes the candidate surviving every final history/expiry
// fence, rather than an earlier candidate that was subsequently discarded.
func finalizeRequestDemandSources(statuses []api.ModelStatus, predictions map[string]int32) {
	for i := range statuses {
		demand := statuses[i].RequestDemand
		if demand == nil {
			continue
		}
		if _, selected := predictions[statuses[i].Name]; !selected {
			demand.Source = "hold"
			if demand.FallbackReason == "" {
				demand.FallbackReason = "forecast did not survive final validity checks"
			}
		} else if demand.Source != "request-demand" {
			demand.Source = "cpu-forecast"
		}
	}
}

type demandPod struct {
	workloaddemand.PodIdentity
	SeenAt time.Time `json:"seenAt"`
}

// Separate owned state keeps the CPU fallback history independent of traffic
// availability. Publication of the state always precedes use of a new forecast.
type demandCPUAccount struct {
	Pod               string    `json:"pod"`
	UID               string    `json:"uid"`
	From              time.Time `json:"from"`
	To                time.Time `json:"to"`
	CompletedRequests float64   `json:"completedRequests"`
	CPUSeconds        float64   `json:"cpuSeconds"`
	PodSeconds        float64   `json:"podSeconds"`
	Coverage          float64   `json:"coverage"`
	SourceAt          time.Time `json:"sourceAt"`
	ReceivedAt        time.Time `json:"receivedAt"`
	BoundaryAveraged  bool      `json:"boundaryAveraged"`
}
type demandCPURejection struct {
	Pod    string    `json:"pod"`
	UID    string    `json:"uid"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Reason string    `json:"reason"`
}

type demandAccounting struct {
	Rejections              []demandCPURejection                `json:"rejections,omitempty"`
	Slot                    time.Time                           `json:"slot"`
	Window                  workloaddemand.Window               `json:"window"`
	Pods                    []demandCPUAccount                  `json:"pods"`
	Reason                  string                              `json:"reason,omitempty"`
	ArrivalCoverage         float64                             `json:"arrivalCoverage"`
	ArrivalBoundaryAveraged bool                                `json:"arrivalBoundaryAveraged"`
	ArrivalPodCoverage      []workloaddemand.PodTrafficCoverage `json:"arrivalPodCoverage,omitempty"`
	NativeSupportFraction   float64                             `json:"nativeSupportFraction"`
	ExpectedPodSeconds      float64                             `json:"expectedPodSeconds"`
}

type demandForecastEvent struct {
	IssuedAt          time.Time                   `json:"issuedAt"`
	AvailableAt       time.Time                   `json:"availableAt,omitempty"`
	ConsumedAt        time.Time                   `json:"consumedAt"`
	Forecasts         []requestforecast.Point     `json:"forecasts"`
	ComputedForecasts []requestforecast.Point     `json:"computedForecasts,omitempty"`
	DroppedIntervals  int                         `json:"droppedIntervals,omitempty"`
	Diagnostics       requestforecast.Diagnostics `json:"diagnostics"`
}

// A bounded history of real Kubernetes reads avoids losing the last causal
// snapshot when the newest read falls just after the interval being assembled.
type demandCapacityContext struct {
	ObservedAt           time.Time `json:"observedAt"`
	Requested            int32     `json:"requested"`
	Ready                float64   `json:"ready"`
	Running              float64   `json:"running"`
	CPURequestMillicores *int64    `json:"cpuRequestMillicores,omitempty"`
}

type demandState struct {
	Version                 int                                     `json:"version"`
	Identity                string                                  `json:"identity"`
	CreatedAt               time.Time                               `json:"createdAt"`
	LastSlot                time.Time                               `json:"lastSlot"`
	LastFitSlot             time.Time                               `json:"lastFitSlot"`
	Accounting              []demandAccounting                      `json:"accounting,omitempty"`
	Estimator               workloaddemand.State                    `json:"estimator"`
	Observations            []requestforecast.Observation           `json:"observations"`
	Forecasts               []requestforecast.Point                 `json:"forecasts"`
	ForecastIssuedAt        time.Time                               `json:"forecastIssuedAt"`
	ForecastAvailableAt     time.Time                               `json:"forecastAvailableAt,omitempty"`
	ForecastConsumedAt      time.Time                               `json:"forecastConsumedAt"`
	ForecastDiagnostics     *requestforecast.Diagnostics            `json:"forecastDiagnostics,omitempty"`
	LastForecastDiagnostics *requestforecast.Diagnostics            `json:"lastForecastDiagnostics,omitempty"`
	ForecastEvents          []demandForecastEvent                   `json:"forecastEvents,omitempty"`
	LastForecastAttemptAt   time.Time                               `json:"lastForecastAttemptAt,omitempty"`
	LastForecastOutcome     string                                  `json:"lastForecastOutcome,omitempty"`
	LastForecastReason      string                                  `json:"lastForecastReason,omitempty"`
	Pods                    []demandPod                             `json:"pods"`
	Context                 map[string]requestforecast.FeatureValue `json:"context"`
	CapacityHistory         []demandCapacityContext                 `json:"capacityHistory,omitempty"`
}

type demandResourceGatherer interface {
	GatherCgroupResources(context.Context, string, labels.Selector, time.Time, time.Time) (*cgroup.WorkloadObservation, error)
	GatherCgroupPodResources(context.Context, string, string, string, time.Time, time.Time) (*cgroup.WorkloadObservation, error)
}

func demandHash(value interface{}) string {
	raw, _ := json.Marshal(value)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func (r *HPAPlusReconciler) loadDemandState(ctx context.Context, instance *api.HPAPlus, model *api.Model, identity string) (*corev1.ConfigMap, demandState, error) {
	name := "hpaplus-demand-" + demandHash([]string{string(instance.UID), model.Name})[:24]
	cm := &corev1.ConfigMap{}
	err := r.reader().Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: name}, cm)
	if kerrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instance.Namespace,
			Labels:          map[string]string{"hpa.plus/state": "request-demand"},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(instance, api.GroupVersion.WithKind("HPAPlus"))}}, Data: map[string]string{}}
		if err = r.Client.Create(ctx, cm); err != nil {
			return nil, demandState{}, err
		}
	} else if err != nil {
		return nil, demandState{}, err
	}
	owner := metav1.GetControllerOf(cm)
	if owner == nil || owner.UID != instance.UID {
		return nil, demandState{}, fmt.Errorf("request-demand state ownership mismatch")
	}
	state := demandState{}
	if raw := cm.Data["state"]; raw != "" {
		if len(raw) > demandStateLimit {
			return nil, state, fmt.Errorf("request-demand state exceeds limit")
		}
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			return nil, state, fmt.Errorf("invalid request-demand state")
		}
	}
	if state.Identity != identity || state.Version != 1 {
		state = demandState{Version: 1, Identity: identity, CreatedAt: r.now(), Estimator: workloaddemand.NewEstimator(identity).Snapshot()}
		// Persist the epoch before waiting for its first closed accounting slot.
		// Otherwise every cold reconciliation recreates CreatedAt=now and the
		// first interval can never become old enough to collect.
		raw, err := json.Marshal(state)
		if err != nil {
			return nil, demandState{}, err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data["state"] = string(raw)
		if err := r.Client.Update(ctx, cm); err != nil {
			return nil, demandState{}, err
		}
	}
	return cm, state, nil
}

func (r *HPAPlusReconciler) saveDemandState(ctx context.Context, cm *corev1.ConfigMap, state demandState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(raw) > demandStateLimit {
		return fmt.Errorf("request-demand state exceeds bounded storage")
	}
	updated := cm.DeepCopy()
	if updated.Data == nil {
		updated.Data = map[string]string{}
	}
	updated.Data["state"] = string(raw)
	return r.Client.Update(ctx, updated)
}

// Retain genuine training samples, together with every intervening missing
// slot. A fixed slot count can otherwise evict the model back into warm-up
// during normal container churn even though fresh usable observations arrive.
// A prolonged outage is still bounded; neither old labels nor gaps are filled.
func retainDemandObservations(rows []requestforecast.Observation, required int) []requestforecast.Observation {
	if required < 24 {
		required = 60
	}
	start, genuine := len(rows), 0
	for start > 0 {
		start--
		if rows[start].ArrivalsPerSecond != nil {
			genuine++
			if genuine >= required {
				break
			}
		}
	}
	// Four history lengths limits storage during sustained measurement loss.
	if bounded := len(rows) - 4*required; bounded > start {
		start = bounded
	}
	// Reserve space for native accounting, forecast provenance and identities in
	// the bounded ConfigMap. Exceeding storage must not block publication of
	// all new measurements and the valid CPU fallback. Warm-up remains honest
	// if an unusually large feature schema cannot retain the required history.
	bytes := 2
	for i := len(rows) - 1; i >= start; i-- {
		raw, _ := json.Marshal(rows[i])
		bytes += len(raw) + 1
		if bytes > demandStateLimit*2/3 {
			start = i + 1
			break
		}
	}
	return append([]requestforecast.Observation(nil), rows[start:]...)
}

func feature(value float64, unit string, source, receipt time.Time) requestforecast.FeatureValue {
	return requestforecast.FeatureValue{Value: &value, Unit: unit, SourceAt: source, ReceivedAt: receipt}
}

// sampled is the model's own sampled CPU history; only forecastTarget
// reconstructedCPU uses it.
func (r *HPAPlusReconciler) processRequestDemand(ctx context.Context, instance *api.HPAPlus, model *api.Model,
	scale *autoscalingv1.Scale, snapshot cpuMetricSnapshot, status *api.ModelStatus, sampled []api.TimestampedSample) (int32, time.Time, bool) {
	diagnostic := &api.RequestDemandStatus{State: "unavailable", Source: "hold", FallbackReason: "request demand has not qualified"}
	status.RequestDemand = diagnostic
	fail := func(reason string) (int32, time.Time, bool) {
		diagnostic.FallbackReason = reason
		return 0, time.Time{}, false
	}
	if r.TrafficPrometheusURL == "" {
		return fail("traffic Prometheus is not configured")
	}
	resources, ok := r.Gatherer.(demandResourceGatherer)
	if !ok {
		return fail("windowed cgroup measurements are unavailable")
	}
	selector, err := labels.Parse(scale.Status.Selector)
	if err != nil || scale.Status.Selector == "" {
		return fail("invalid workload selector")
	}
	object := &unstructured.Unstructured{}
	object.SetAPIVersion(instance.Spec.ScaleTargetRef.APIVersion)
	object.SetKind(instance.Spec.ScaleTargetRef.Kind)
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: instance.Spec.ScaleTargetRef.Name}, object); err != nil {
		return fail("workload configuration unavailable")
	}
	template, _, _ := unstructured.NestedMap(object.Object, "spec", "template")
	identity := demandHash([]interface{}{string(instance.UID), scaleTargetIdentity(instance, scale), model, template, demandMeasurementSource, r.TrafficPrometheusURL})
	cm, state, err := r.loadDemandState(ctx, instance, model, identity)
	if err != nil {
		return fail("request-demand state unavailable")
	}
	estimator, err := workloaddemand.Restore(identity, state.Estimator)
	if err != nil {
		return fail("request-demand state invalid")
	}
	now := r.now()
	pods := &corev1.PodList{}
	if err := r.reader().List(ctx, pods, client.InNamespace(instance.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return fail("workload pod identities unavailable")
	}
	podset := map[string]demandPod{}
	for _, p := range state.Pods {
		if p.SeenAt.After(now.Add(-demandPublicationDelay).UTC().Truncate(30 * time.Second).Add(-30 * time.Second)) {
			podset[p.UID] = p
		}
	}
	ready, running := 0., 0.
	homogeneous := true
	templateContainers, _, _ := unstructured.NestedSlice(template, "spec", "containers")
	expectedImages := map[string]string{}
	for _, item := range templateContainers {
		if c, ok := item.(map[string]interface{}); ok {
			name, _ := c["name"].(string)
			image, _ := c["image"].(string)
			expectedImages[name] = image
		}
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded {
			continue
		}
		if len(p.Spec.Containers) != len(expectedImages) {
			homogeneous = false
		}
		request := int64(0)
		for _, container := range p.Spec.Containers {
			q := container.Resources.Requests[corev1.ResourceCPU]
			request += q.MilliValue()
			if expectedImages[container.Name] != container.Image {
				homogeneous = false
			}
		}
		if snapshot.RequestPerPodMillicores == nil || request != *snapshot.RequestPerPodMillicores {
			homogeneous = false
		}
		podset[string(p.UID)] = demandPod{workloaddemand.PodIdentity{Name: p.Name, UID: string(p.UID)}, now}
		if p.Status.Phase == corev1.PodRunning {
			running++
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready++
			}
		}
	}
	capacityObservedAt := r.now()
	state.Pods = nil
	scope := workloaddemand.Scope{Namespace: instance.Namespace}
	for _, p := range podset {
		state.Pods = append(state.Pods, p)
		scope.Pods = append(scope.Pods, p.PodIdentity)
	}
	sort.Slice(state.Pods, func(i, j int) bool { return state.Pods[i].UID < state.Pods[j].UID })
	sort.Slice(scope.Pods, func(i, j int) bool { return scope.Pods[i].UID < scope.Pods[j].UID })
	if len(scope.Pods) == 0 {
		return fail("no workload pods")
	}
	// A brief publication delay permits the scrape containing the closed window.
	// The slot is immutable after its first acquisition, including missing results.
	slot := now.Add(-demandPublicationDelay).UTC().Truncate(30 * time.Second)
	changed := false
	if slot.After(state.LastSlot) && !slot.Add(-30*time.Second).Before(state.CreatedAt) {
		state.LastSlot = slot
		coverageUnavailable := 0.
		row := requestforecast.Observation{At: slot.Add(-30 * time.Second), Until: slot, ReceivedAt: now, Features: map[string]requestforecast.FeatureValue{}, ArrivalCoverage: &coverageUnavailable}
		for name, value := range state.Context {
			if !value.SourceAt.After(slot) {
				row.Features[name] = value
			}
		}
		selectDemandCapacityFeatures(&row, state.CapacityHistory)
		cpuWindow, cpuWindowErr := resources.GatherCgroupResources(ctx, instance.Namespace, selector, row.At, row.Until)
		// A short-lived pod can start and disappear between reconciliations.
		// Retained node inventory must also scope its traffic, otherwise a
		// perfectly aligned subset could be mistaken for the whole deployment.
		scope = demandWindowScope(scope, cpuWindow)
		for _, pod := range pods.Items {
			if pod.CreationTimestamp.Time.After(row.Until) {
				if scope.Lifetimes == nil {
					scope.Lifetimes = map[string]workloaddemand.PodLifetime{}
				}
				scope.Lifetimes[string(pod.UID)] = workloaddemand.PodLifetime{Known: true}
			}
		}
		var traffic workloaddemand.TrafficWindow
		var readErr error
		if cpuWindowErr != nil || cpuWindow == nil || !cpuWindow.InventoryComplete {
			readErr = fmt.Errorf("complete deployment runtime inventory unavailable")
		} else {
			traffic, readErr = (&workloaddemand.TrafficReader{URL: r.TrafficPrometheusURL, Now: r.now}).ReadWindow(ctx, scope, *model.RequestDemand, row.At, row.Until)
		}
		row.ReceivedAt = r.now()
		if readErr == nil {
			for _, point := range traffic.ArrivalIntervals {
				row.ArrivalIntervals = append(row.ArrivalIntervals, requestforecast.ArrivalInterval{At: point.From, Until: point.To, ReceivedAt: point.AvailableAt, Rate: point.Rate, Coverage: point.Coverage, BoundaryAveraged: point.BoundaryAveraged})
			}
			if traffic.ArrivalCoverage >= .95 && cpuWindowErr == nil && cpuWindow != nil {
				v := traffic.ArrivalsPerSecond
				row.ArrivalsPerSecond = &v
			}
			coverage := traffic.ArrivalCoverage
			row.ArrivalCoverage = &coverage
			row.ArrivalBoundaryAveraged = traffic.ArrivalBoundaryAveraged
			for name, v := range traffic.Features {
				value := requestforecast.FeatureValue{Value: v.Value, SourceAt: v.SourceAt, ReceivedAt: v.ReceivedAt, Unit: v.Unit}
				if v.MeasurementWindow != nil {
					value.MeasurementWindow = &requestforecast.MeasurementWindow{From: v.MeasurementWindow.From, To: v.MeasurementWindow.To}
				}
				row.Features[name] = value
			}
			evidence := collectDemandNativeAccounting(ctx, resources, instance.Namespace, identity, slot, traffic, cpuWindow)
			account := evidence.Window
			if cpuWindowErr != nil || cpuWindow == nil {
				account.Qualified = false
				account.Reason = "deployment runtime inventory unavailable"
			}
			if !homogeneous {
				account.Qualified = false
				account.Reason = "mixed pod capacity or workload revision"
			}
			// Preserve the accounting contamination guard: errors per processed
			// completion, not errors/all HTTP or gRPC outcomes. Cheap rejected
			// requests do not become equally costly completed processing work.
			errors, completions := traffic.Features["traffic_errors"], traffic.Features["traffic_completions"]
			if errors.Value != nil && completions.Value != nil && *completions.Value > 0 && *errors.Value / *completions.Value > .01 {
				account.Qualified = false
				account.Reason = "failed requests contaminate completed-work accounting"
			}
			account.AvailableAt = r.now()
			estimator.Observe(account, r.now())
			evidence.Window = account
			state.Accounting = append(state.Accounting, evidence)
			if len(state.Accounting) > 32 {
				state.Accounting = state.Accounting[len(state.Accounting)-32:]
			}
		} else {
			diagnostic.FallbackReason = "traffic observation unavailable"
			account := workloaddemand.Window{ID: identity + "/" + slot.Format(time.RFC3339), Start: row.At, End: row.Until, AvailableAt: r.now(), Qualified: false, Reason: readErr.Error()}
			estimator.Observe(account, r.now())
			state.Accounting = append(state.Accounting, demandAccounting{Slot: slot, Window: account, Reason: readErr.Error()})
			if len(state.Accounting) > 32 {
				state.Accounting = state.Accounting[len(state.Accounting)-32:]
			}
		}
		if state.Context == nil {
			state.Context = map[string]requestforecast.FeatureValue{}
		}
		recordDemandResourceFeatures(state.Context, &row, cpuWindow, cpuWindowErr, r.now())
		row.ReceivedAt = r.now()
		state.Observations = append(state.Observations, row)
		historySize := 60
		if model.XGBoost != nil && model.XGBoost.HistorySize > 0 {
			historySize = int(model.XGBoost.HistorySize)
		}
		state.Observations = retainDemandObservations(state.Observations, historySize)
		// These Kubernetes states were observed now, so only later intervals may use them.
		if state.Context == nil {
			state.Context = map[string]requestforecast.FeatureValue{}
		}
		recordDemandCapacityContext(&state, demandCapacityContext{ObservedAt: capacityObservedAt,
			Requested: scale.Spec.Replicas, Ready: ready, Running: running,
			CPURequestMillicores: snapshot.RequestPerPodMillicores})
		state.Estimator = estimator.Snapshot()
		changed = true
	}
	estimate := estimator.Estimate(r.now())
	diagnostic.State, diagnostic.QualifiedSamples, diagnostic.Version = estimate.State, estimate.QualifiedWindows, estimate.Version
	diagnostic.CPUSecondsPerRequest, diagnostic.BackgroundCoresPerPod = estimate.CPUSecondsPerRequest, estimate.BackgroundCPUPerPod
	if !estimate.LastObservationAt.IsZero() {
		diagnostic.LastObservationAt = &metav1.Time{Time: estimate.LastObservationAt}
	}
	if !estimate.FreshUntil.IsZero() {
		diagnostic.FreshUntil = &metav1.Time{Time: estimate.FreshUntil}
	}
	reconstructed := reconstructsCPU(model)
	var reconstructedRows []api.TimestampedSample
	reconstructionFallbacks, reconstructedSize := 0, 60
	if model.XGBoost != nil && model.XGBoost.HistorySize > 0 {
		reconstructedSize = int(model.XGBoost.HistorySize)
	}
	if reconstructed {
		// State and freshness describe the reconstructed path; the cost estimator
		// keeps learning but does not gate it.
		diagnostic.State, diagnostic.FreshUntil, diagnostic.Target = "learning", nil, api.RequestDemandTargetReconstructedCPU
		reconstructedRows, reconstructionFallbacks = reconstructedHistory(sampled, state.Observations, reconstructedSize)
		// Fit once per closed slot, after that slot's own CPU sample exists.
		if state.LastFitSlot.Before(state.LastSlot) && len(reconstructedRows) >= reconstructedSize &&
			!reconstructedRows[len(reconstructedRows)-1].Time.Time.Before(state.LastSlot.Add(-reconstructedWindowMatch)) {
			requested := r.now()
			result, fitErr := r.runReconstructedForecast(ctx, instance, model, reconstructedRows, scaleTargetIdentity(instance, scale), status)
			state.LastForecastAttemptAt = requested
			state.LastForecastOutcome, state.LastForecastReason = "ready", ""
			state.LastForecastDiagnostics = nil
			if fitErr != nil {
				state.LastForecastOutcome, state.LastForecastReason = "unavailable", "worker_or_protocol_failure"
				log.FromContext(ctx).Info("Reconstructed forecast unavailable", "model", model.Name, "reason", "worker_or_protocol_failure")
			} else {
				publishRequestForecast(&state, result, requested, r.now())
			}
			state.LastFitSlot = state.LastSlot
			changed = true
		}
	} else if state.LastFitSlot.Before(state.LastSlot) && len(state.Observations) >= 60 {
		requested := r.now()
		// Start at the next unstarted interval. An extra unconditional interval
		// would shift the configured three-step forecast an additional 30s out.
		config, err := requestforecast.ConfigFromModel(model)
		if err == nil {
			frame := config.ForecastIntervalSeconds
			if frame == 0 {
				frame = config.IntervalSeconds
			}
			first := requested.UTC().Truncate(time.Duration(frame) * time.Second).Add(time.Duration(frame) * time.Second)
			units := map[string]string{"cpu_usage": "cores", "memory_usage": "bytes", "cpu_throttled": "seconds/second", "cpu_pressure": "seconds/second", "requested_replicas": "replicas", "ready_replicas": "replicas", "running_replicas": "replicas", "cpu_request_per_pod": "cores", "traffic_arrivals": "requests/second", "traffic_completions": "requests/second", "traffic_errors": "errors/second", "traffic_error_rate": "ratio", "traffic_latency_p95": "seconds"}
			for _, extra := range model.RequestDemand.ExtraMetrics {
				units[extra.Name] = extra.Unit
			}
			for name, unit := range units {
				maximumAge := 120
				for _, extra := range model.RequestDemand.ExtraMetrics {
					if extra.Name == name {
						maximumAge = extra.MaxAgeSeconds
						if maximumAge == 0 {
							maximumAge = 30
						}
					}
				}
				config.Features = append(config.Features, requestforecast.FeatureSpec{Name: name, Unit: unit, MaxAgeSeconds: maximumAge})
			}
			// Keep the runtime schema identical to replay/tuning: feature
			// subsampling must never depend on Go map iteration order.
			sort.Slice(config.Features, func(i, j int) bool { return config.Features[i].Name < config.Features[j].Name })
			result, fitErr := r.runRequestForecast(ctx, model, state.Observations, requested, first, config, status)
			state.LastForecastAttemptAt = requested
			state.LastForecastOutcome, state.LastForecastReason = result.Status, result.Reason
			state.LastForecastDiagnostics = nil
			if fitErr == nil {
				state.LastForecastDiagnostics = &result.Diagnostics
			}
			if fitErr != nil {
				state.LastForecastOutcome, state.LastForecastReason = "unavailable", "worker_or_protocol_failure"
			}
			state.LastFitSlot = state.LastSlot
			changed = true
			if fitErr == nil && result.Status == "ready" {
				publishRequestForecast(&state, result, requested, r.now())
			} else if fitErr != nil {
				log.FromContext(ctx).Info("Request forecast unavailable", "model", model.Name, "reason", "worker_or_protocol_failure")
			}
		}
	}
	if changed {
		if err := r.saveDemandState(ctx, cm, state); err != nil {
			return fail("request-demand publication failed")
		}
	}
	if !homogeneous {
		return fail("heterogeneous pod capacity or workload revision")
	}
	if reconstructed {
		recent := r.recentReconstructedWindow(ctx, instance, scale, state.Observations, sampled)
		return r.reconstructedRecommendation(scopedPolicy(model, pods.Items), scale, snapshot, state, sampled, reconstructedRows, reconstructionFallbacks, reconstructedSize,
			syncPeriodForInstance(instance), recent, status, fail)
	}
	if estimate.State != "qualified" {
		return fail("CPU-cost estimate " + estimate.State)
	}
	if snapshot.RequestPerPodMillicores == nil || snapshot.TargetUtilizationPercentage == nil {
		return fail("CPU capacity configuration unavailable")
	}
	if model.PredictionMaxAge == nil || state.ForecastConsumedAt.IsZero() || !r.now().Before(state.ForecastConsumedAt.Add(time.Duration(*model.PredictionMaxAge)*time.Millisecond)) {
		return fail("request forecast missing or expired")
	}
	replicas, deadline, err := requestReplicas(state.Forecasts, estimate, effectivePolicy(*model.PredictivePolicy, pods.Items), float64(*snapshot.RequestPerPodMillicores)/1000, float64(*snapshot.TargetUtilizationPercentage)/100, scale.Spec.Replicas, r.now())
	if err != nil {
		return fail("request forecast interval unavailable")
	}
	ageDeadline := state.ForecastConsumedAt.Add(time.Duration(*model.PredictionMaxAge) * time.Millisecond)
	if ageDeadline.Before(deadline) {
		deadline = ageDeadline
	}
	if estimate.FreshUntil.Before(deadline) {
		deadline = estimate.FreshUntil
	}
	diagnostic.Source = "request-demand"
	diagnostic.FallbackReason = ""
	status.State = "Ready"
	status.HistorySamples = 0
	for _, observation := range state.Observations {
		if observation.ArrivalsPerSecond != nil {
			status.HistorySamples++
		}
	}
	status.ForecastReplicas = &replicas
	status.ConsumedUntil = &metav1.Time{Time: state.ForecastConsumedAt}
	status.ExpiresAt = &metav1.Time{Time: deadline}
	return replicas, deadline, true
}

// selectDemandCapacityFeatures uses the most recent read actually available by
// the accounting boundary. The original timestamp is retained, so stale gauge
// values are masked by the existing feature-age policy instead of backdated.
func selectDemandCapacityFeatures(row *requestforecast.Observation, history []demandCapacityContext) {
	var selected *demandCapacityContext
	for i := range history {
		sample := &history[i]
		if !sample.ObservedAt.After(row.Until) && (selected == nil || sample.ObservedAt.After(selected.ObservedAt)) {
			selected = sample
		}
	}
	if selected == nil {
		return
	}
	if row.Features == nil {
		row.Features = map[string]requestforecast.FeatureValue{}
	}
	for name, value := range demandCapacityValues(*selected) {
		row.Features[name] = value
	}
}

func demandCapacityValues(sample demandCapacityContext) map[string]requestforecast.FeatureValue {
	values := map[string]requestforecast.FeatureValue{
		"requested_replicas":  feature(float64(sample.Requested), "replicas", sample.ObservedAt, sample.ObservedAt),
		"ready_replicas":      feature(sample.Ready, "replicas", sample.ObservedAt, sample.ObservedAt),
		"running_replicas":    feature(sample.Running, "replicas", sample.ObservedAt, sample.ObservedAt),
		"cpu_request_per_pod": {SourceAt: sample.ObservedAt, ReceivedAt: sample.ObservedAt, Unit: "cores"},
	}
	if sample.CPURequestMillicores != nil {
		values["cpu_request_per_pod"] = feature(float64(*sample.CPURequestMillicores)/1000, "cores", sample.ObservedAt, sample.ObservedAt)
	}
	return values
}

func recordDemandCapacityContext(state *demandState, sample demandCapacityContext) {
	if sample.CPURequestMillicores != nil {
		value := *sample.CPURequestMillicores
		sample.CPURequestMillicores = &value
	}
	state.CapacityHistory = append(state.CapacityHistory, sample)
	if len(state.CapacityHistory) > 16 {
		state.CapacityHistory = state.CapacityHistory[len(state.CapacityHistory)-16:]
	}
	if state.Context == nil {
		state.Context = map[string]requestforecast.FeatureValue{}
	}
	for name, value := range demandCapacityValues(sample) {
		state.Context[name] = value
	}
}

// publishRequestForecast evaluates each target at its actual availability time.
// A fit crossing its first boundary must not discard later, still-unstarted
// intervals. Started intervals are retained as raw evidence, never published as
// new future forecasts; a previously issued current interval keeps its coverage.
func publishRequestForecast(state *demandState, result requestforecast.Result, requested, available time.Time) {
	future := make([]requestforecast.Point, 0, len(result.Forecasts))
	for _, point := range result.Forecasts {
		if !point.At.Before(available) {
			future = append(future, point)
		}
	}
	state.ForecastEvents = append(state.ForecastEvents, demandForecastEvent{
		IssuedAt: requested, AvailableAt: available, ConsumedAt: result.TrainedThrough,
		Forecasts:         append([]requestforecast.Point{}, future...),
		ComputedForecasts: append([]requestforecast.Point(nil), result.Forecasts...),
		DroppedIntervals:  len(result.Forecasts) - len(future), Diagnostics: result.Diagnostics,
	})
	if len(state.ForecastEvents) > 10 {
		state.ForecastEvents = state.ForecastEvents[len(state.ForecastEvents)-10:]
	}
	if len(future) == 0 {
		state.LastForecastOutcome = "unavailable"
		state.LastForecastReason = "no unstarted forecast intervals remained at publication"
		return
	}
	state.Forecasts = mergeRequestInterval(state.Forecasts, future, available)
	state.ForecastIssuedAt = requested
	state.ForecastAvailableAt = available
	state.ForecastConsumedAt = result.TrainedThrough
	state.ForecastDiagnostics = &result.Diagnostics
}

// runRequestForecast reports the same end-to-end attempt accounting as the
// CPU forecast path. These fields describe the latest forecast invocation;
// RequestDemand.Source separately describes the currently usable scale source.
// Recording an outcome never changes request cadence, reuse or failure policy.
func (r *HPAPlusReconciler) runRequestForecast(ctx context.Context, model *api.Model, observations []requestforecast.Observation, requested, first time.Time, config requestforecast.Config, status *api.ModelStatus) (requestforecast.Result, error) {
	status.LastAttemptTime = &metav1.Time{Time: requested}
	started := time.Now()
	result, err := requestforecast.Predict(ctx, r.WorkloadRunner, model, observations, requested, first, config)
	outcome := "success"
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
		outcome = "timeout"
	case errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled):
		outcome = "canceled"
	case err != nil:
		outcome = "failed"
	case result.Status == "warming":
		outcome = "insufficient_history"
	case result.Status != "ready":
		outcome = "failed"
	}
	elapsed := time.Since(started)
	duration := elapsed.Milliseconds()
	status.LastRunDurationMilliseconds, status.LastRunOutcome = &duration, outcome
	if outcome == "success" {
		status.LastSuccessTime = &metav1.Time{Time: r.now()}
	}
	r.Telemetry.ObserveModelRun(model.Type, outcome, elapsed)
	return result, err
}

// A closed window contributes its own measured values or explicit missing
// values. Previous real observations remain intact, but cannot stand in for a
// failed newer acquisition merely because their source timestamp is still fresh.
func recordDemandResourceFeatures(context map[string]requestforecast.FeatureValue, row *requestforecast.Observation, cpu *cgroup.WorkloadObservation, readErr error, received time.Time) {
	units := map[string]string{"cpu_usage": "cores", "memory_usage": "bytes", "cpu_throttled": "seconds/second", "cpu_pressure": "seconds/second"}
	values := make(map[string]requestforecast.FeatureValue, len(units))
	for name, unit := range units {
		values[name] = requestforecast.FeatureValue{SourceAt: row.Until, ReceivedAt: received, Unit: unit}
	}
	if readErr == nil && cpu != nil && cpu.Complete && !cpu.From.IsZero() && cpu.To.After(cpu.From) && !cpu.To.After(row.Until) && !cpu.SourceAt.IsZero() && !cpu.SourceAt.After(received) && (cpu.ReceivedAt.IsZero() || !cpu.ReceivedAt.Before(cpu.SourceAt) && !cpu.ReceivedAt.After(received)) {
		receipt := cpu.ReceivedAt
		if receipt.IsZero() {
			receipt = received
		}
		windowed := func(value float64, unit string) requestforecast.FeatureValue {
			v := feature(value, unit, cpu.SourceAt, receipt)
			v.MeasurementWindow = &requestforecast.MeasurementWindow{From: cpu.From, To: cpu.To}
			return v
		}
		duration := cpu.To.Sub(cpu.From).Seconds()
		values["cpu_usage"] = windowed(cpu.CPUSeconds/duration, "cores")
		if cpu.MemoryBytes != nil {
			values["memory_usage"] = feature(float64(*cpu.MemoryBytes), "bytes", cpu.MemorySourceAt, receipt)
		}
		if cpu.ThrottledSeconds != nil {
			values["cpu_throttled"] = windowed(*cpu.ThrottledSeconds/duration, "seconds/second")
		}
		if cpu.PressureSomeSeconds != nil {
			values["cpu_pressure"] = windowed(*cpu.PressureSomeSeconds/duration, "seconds/second")
		}
	}
	for name, value := range values {
		context[name] = value
		// Later source windows may provide context to a later observation, but
		// cannot appear as an already observed value in this closed interval.
		row.Features[name] = requestforecast.FeatureValue{SourceAt: row.Until, ReceivedAt: received, Unit: units[name]}
		if !value.SourceAt.After(row.Until) || value.MeasurementWindow != nil {
			row.Features[name] = value
		}
	}
}

// collectDemandNativeAccounting never substitutes a fleet CPU total for partial
// completion support. Every accepted count is paired with CPU and pod-seconds
// over exactly the same native interval. Selection depends on measurement
// validity, never utilization or model output.
func collectDemandNativeAccounting(ctx context.Context, resources demandResourceGatherer, namespace, identity string, slot time.Time, traffic workloaddemand.TrafficWindow, retained ...*cgroup.WorkloadObservation) demandAccounting {
	readCPU := resources.GatherCgroupPodResources
	if len(retained) > 0 && retained[0] != nil && len(retained[0].NativeSnapshots) > 0 {
		batch := retained[0]
		readCPU = func(_ context.Context, ns, pod, uid string, from, to time.Time) (*cgroup.WorkloadObservation, error) {
			views := make([]cgroup.Snapshot, 0, len(batch.NativeSnapshots))
			found := false
			for _, original := range batch.NativeSnapshots {
				view := original
				view.Pods = nil
				for _, value := range original.Pods {
					if value.Namespace == ns && value.Name == pod && value.UID == uid {
						view.Pods = append(view.Pods, value)
						found = true
					}
				}
				views = append(views, view)
			}
			if !found {
				return nil, fmt.Errorf("native pod identity unavailable in retained batch")
			}
			return cgroup.Integrate(views, ns, labels.Everything(), from, to, batch.ReceivedAt)
		}
	}
	evidence := demandAccounting{Slot: slot, ArrivalCoverage: traffic.ArrivalCoverage, ArrivalBoundaryAveraged: traffic.ArrivalBoundaryAveraged, ArrivalPodCoverage: traffic.ArrivalPodCoverage, ExpectedPodSeconds: traffic.ExpectedPodSeconds}
	account := workloaddemand.Window{ID: identity + "/" + slot.Format(time.RFC3339), Start: slot.Add(-30 * time.Second), End: slot, Coverage: traffic.Coverage, Qualified: traffic.Qualified}
	if !account.Qualified {
		account.Reason = "incomplete fleet native completion support: " + traffic.Reason
	}
	first := true
	appendCPU := func(interval workloaddemand.PodCounterInterval, cpu *cgroup.WorkloadObservation) {
		value := demandCPUAccount{Pod: interval.Pod, UID: interval.UID, From: interval.Start, To: interval.End, CompletedRequests: interval.CompletedRequests, CPUSeconds: cpu.CPUSeconds, PodSeconds: cpu.RunningPodSeconds, Coverage: cpu.Coverage, SourceAt: cpu.SourceAt, ReceivedAt: cpu.ReceivedAt, BoundaryAveraged: cpu.BoundaryAveraged}
		if len(evidence.Pods) > 0 && evidence.Pods[len(evidence.Pods)-1].UID == value.UID && evidence.Pods[len(evidence.Pods)-1].To.Equal(value.From) {
			previous := &evidence.Pods[len(evidence.Pods)-1]
			previous.To = value.To
			previous.CompletedRequests += value.CompletedRequests
			previous.CPUSeconds += value.CPUSeconds
			previous.PodSeconds += value.PodSeconds
			previous.Coverage = math.Min(previous.Coverage, value.Coverage)
			previous.BoundaryAveraged = previous.BoundaryAveraged || value.BoundaryAveraged
			if value.SourceAt.After(previous.SourceAt) {
				previous.SourceAt = value.SourceAt
			}
			if value.ReceivedAt.After(previous.ReceivedAt) {
				previous.ReceivedAt = value.ReceivedAt
			}
		} else {
			evidence.Pods = append(evidence.Pods, value)
		}
		account.CompletedRequests += interval.CompletedRequests
		account.CPUSeconds += cpu.CPUSeconds
		account.PodSeconds += cpu.RunningPodSeconds
		if first || interval.Start.Before(account.Start) {
			account.Start = interval.Start
		}
		if first || interval.End.After(account.End) {
			account.End = interval.End
		}
		first = false
	}
	validCPU := func(cpu *cgroup.WorkloadObservation, err error) bool {
		return err == nil && cpu != nil && cpu.Complete && cpu.Coverage >= .95
	}
	rejected := func(interval workloaddemand.PodCounterInterval, cpu *cgroup.WorkloadObservation, err error) {
		reason := "incomplete native CPU accounting"
		if err != nil {
			reason = "native CPU collection failed"
		} else if cpu != nil && len(cpu.MissingReasons) > 0 {
			reason = strings.Join(cpu.MissingReasons, "; ")
		}
		if len(evidence.Rejections) > 0 {
			previous := &evidence.Rejections[len(evidence.Rejections)-1]
			if previous.UID == interval.UID && previous.Reason == reason && previous.To.Equal(interval.Start) {
				previous.To = interval.End
				return
			}
		}
		if len(evidence.Rejections) < 64 {
			evidence.Rejections = append(evidence.Rejections, demandCPURejection{Pod: interval.Pod, UID: interval.UID, From: interval.Start, To: interval.End, Reason: reason})
		}
	}
	acceptedGroups := 0
	for _, interval := range traffic.PerPodIntervals {
		cpu, err := readCPU(ctx, namespace, interval.Pod, interval.UID, interval.Start, interval.End)
		if validCPU(cpu, err) {
			appendCPU(interval, cpu)
			acceptedGroups++
			continue
		}
		accepted := false
		pairsRead := 0
		// A startup/end boundary may spoil only one short native pair. Retain
		// complete later/earlier pairs, and persist each rejected part as a gap.
		for _, pair := range traffic.PerPodNativeIntervals {
			if pair.UID != interval.UID || pair.Start.Before(interval.Start) || pair.End.After(interval.End) {
				continue
			}
			part, e := readCPU(ctx, namespace, pair.Pod, pair.UID, pair.Start, pair.End)
			pairsRead++
			if validCPU(part, e) {
				appendCPU(pair, part)
				accepted = true
			} else {
				rejected(pair, part, e)
			}
		}
		if accepted {
			acceptedGroups++
		} else if pairsRead == 0 {
			rejected(interval, cpu, err)
		}
	}
	if len(traffic.PerPodIntervals) == 0 || acceptedGroups != len(traffic.PerPodIntervals) {
		account.Qualified = false
		account.Reason = "native CPU/completion support missing for an active fleet member"
		if len(traffic.PerPodIntervals) > 0 {
			account.Coverage = math.Min(account.Coverage, float64(acceptedGroups)/float64(len(traffic.PerPodIntervals)))
		}
	}
	if traffic.ExpectedPodSeconds > 0 {
		evidence.NativeSupportFraction = account.PodSeconds / traffic.ExpectedPodSeconds
	}
	evidence.Window = account
	return evidence
}

func demandWindowScope(scope workloaddemand.Scope, window *cgroup.WorkloadObservation) workloaddemand.Scope {
	seen := map[string]bool{}
	for _, pod := range scope.Pods {
		seen[pod.UID] = true
	}
	if window != nil {
		inventory := window.Inventory
		if len(inventory) == 0 {
			inventory = window.Pods
		}
		scope.Lifetimes = map[string]workloaddemand.PodLifetime{}
		for _, pod := range inventory {
			if pod.Namespace == scope.Namespace && pod.Name != "" && pod.UID != "" && !seen[pod.UID] {
				scope.Pods = append(scope.Pods, workloaddemand.PodIdentity{Name: pod.Name, UID: pod.UID})
				seen[pod.UID] = true
			}
			if pod.Namespace == scope.Namespace && pod.UID != "" {
				life := workloaddemand.PodLifetime{Known: len(pod.Containers) > 0}
				if pod.CreatedAt.After(window.To) {
					life.Known = true
					scope.Lifetimes[pod.UID] = life
					continue
				}
				for _, container := range pod.Containers {
					if (container.ID == "" || container.StartedAt.IsZero()) && container.FinishedAt != nil && !container.FinishedAt.After(window.From) {
						continue
					}
					if (container.ID == "" || container.StartedAt.IsZero()) && container.FinishedAt == nil && container.EndUpperBound != nil && !container.EndUpperBound.After(window.From) {
						continue
					}
					if container.ID == "" || container.StartedAt.IsZero() {
						life.Known = false
						life.Reason = "unknown runtime epoch"
						continue
					}
					interval := workloaddemand.TimeInterval{From: container.StartedAt}
					life.Boundaries = append(life.Boundaries, container.StartedAt)
					if container.FinishedAt != nil {
						interval.To = *container.FinishedAt
						life.Boundaries = append(life.Boundaries, *container.FinishedAt)
					} else if container.EndUpperBound != nil {
						interval.To = *container.EndUpperBound
						life.Boundaries = append(life.Boundaries, *container.EndUpperBound)
						life.Reason = "exit bounded by observed absence; unknown tail remains required"
					}
					life.Intervals = append(life.Intervals, interval)
				}
				scope.Lifetimes[pod.UID] = life
			}
		}
		if window.To.After(window.From) {
			// Long-retired identities are proven absent and need no metric
			// query. Keep their inventory record, but do not let retained node
			// history expand the bounded query population indefinitely.
			filtered := scope.Pods[:0]
			for _, pod := range scope.Pods {
				life, known := scope.Lifetimes[pod.UID]
				active := !known || !life.Known
				for _, interval := range life.Intervals {
					if interval.From.Before(window.To) && (interval.To.IsZero() || interval.To.After(window.From)) {
						active = true
						break
					}
				}
				if active {
					filtered = append(filtered, pod)
				}
			}
			scope.Pods = filtered
		}
	}
	sort.Slice(scope.Pods, func(i, j int) bool { return scope.Pods[i].UID < scope.Pods[j].UID })
	return scope
}

func mergeRequestInterval(old, fresh []requestforecast.Point, now time.Time) []requestforecast.Point {
	if len(fresh) == 0 {
		return nil
	}
	out := make([]requestforecast.Point, 0, len(fresh)+3)
	for _, p := range old {
		if !p.Until.After(now) || !p.At.Before(fresh[0].At) {
			continue
		}
		if p.Until.After(fresh[0].At) {
			p.Until = fresh[0].At
		}
		out = append(out, p)
	}
	return append(out, fresh...)
}

func requestReplicas(points []requestforecast.Point, estimate workloaddemand.Estimate, policy api.PredictivePolicy, request, target float64, current int32, now time.Time) (int32, time.Time, error) {
	var value float64
	known := false
	upcoming := false
	var deadline time.Time
	consider := func(t time.Time) {
		if t.After(now) && (deadline.IsZero() || t.Before(deadline)) {
			deadline = t
		}
	}
	for _, p := range points {
		consider(p.At)
		consider(p.Until)
		consider(p.At.Add(-time.Duration(policy.ScaleUpLeadSeconds) * time.Second))
		if !now.Before(p.At) && now.Before(p.Until) {
			known = true
			if p.Value > value {
				value = p.Value
			}
		}
		if p.At.After(now) && !p.At.After(now.Add(time.Duration(policy.ScaleUpLeadSeconds)*time.Second)) {
			upcoming = true
			if p.Value > value {
				value = p.Value
			}
		}
	}
	if deadline.IsZero() || (!known && !upcoming) {
		return 0, time.Time{}, fmt.Errorf("no live request forecast")
	}
	ratio, err := workloaddemand.RequiredReplicas(value, request, target, estimate, now)
	if err != nil || ratio > math.MaxInt32 {
		return 0, time.Time{}, fmt.Errorf("invalid learned capacity")
	}
	desired := int32(math.Ceil(ratio))
	if !known && desired < current {
		desired = current
	}
	if desired > current && current > 0 && ratio/float64(current) <= 1+policy.EffectiveTolerance() {
		desired = current
	}
	return desired, deadline, nil
}
