// Package ui serves a read-only local web view of HPAPlus objects: what each one
// decided, why, and the recent CPU, replica and forecast history behind it.
// It only reads Kubernetes objects with the caller's credentials.
package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/history"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IdleAfter is how old the latest decision may be before the overview reports
// that the HPAPlus is no longer being reconciled.
const IdleAfter = 5 * time.Minute

// PartialMetricsGrace is how long missing pod metrics count as normal: new
// pods report CPU only after their first scrape.
const PartialMetricsGrace = 3 * time.Minute

// trendSamples bounds the history returned for an overview sparkline.
const trendSamples = 120

// Summary is one HPAPlus as the overview lists it.
type Summary struct {
	Namespace   string         `json:"namespace"`
	Name        string         `json:"name"`
	Target      string         `json:"target"`
	Mode        string         `json:"mode"`
	Replicas    int32          `json:"replicas"`
	Ready       *int32         `json:"ready,omitempty"`
	Desired     *int32         `json:"desired,omitempty"`
	Min         int32          `json:"min"`
	Max         int32          `json:"max"`
	DecidedBy   string         `json:"decidedBy,omitempty"`
	ScaleWrite  string         `json:"scaleWrite,omitempty"`
	EvaluatedAt *time.Time     `json:"evaluatedAt,omitempty"`
	Health      string         `json:"health"`
	Reason      string         `json:"reason,omitempty"`
	Models      []ModelSummary `json:"models"`
}

// Capacity converts CPU into pods: each pod requests RequestCores and is meant
// to run at TargetPercent of it.
type Capacity struct {
	RequestCores  float64 `json:"requestCores"`
	TargetPercent int32   `json:"targetPercent"`
}

// ModelSummary is a model's state as the latest reconciliation reported it.
type ModelSummary struct {
	Name             string     `json:"name"`
	Type             string     `json:"type"`
	State            string     `json:"state"`
	Samples          int64      `json:"samples"`
	Required         int64      `json:"required"`
	ForecastReplicas *int32     `json:"forecastReplicas,omitempty"`
	LastRunOutcome   string     `json:"lastRunOutcome,omitempty"`
	LastSuccess      *time.Time `json:"lastSuccess,omitempty"`
	ExpiresAt        *time.Time `json:"expiresAt,omitempty"`
	Source           string     `json:"source,omitempty"`
	DecidedBy        string     `json:"decidedBy,omitempty"`
	FloorReplicas    int32      `json:"floorReplicas,omitempty"`
	ForecastOnly     int32      `json:"forecastOnly,omitempty"`
	MeasuredCores    float64    `json:"measuredCores,omitempty"`
	CPUPerRequest    float64    `json:"cpuSecondsPerRequest,omitempty"`
	FallbackReason   string     `json:"fallbackReason,omitempty"`
}

// Sample is one stored CPU history window. CapacityCores is the CPU the pods of
// that window could use at the target utilization.
type Sample struct {
	Time          time.Time `json:"t"`
	Cores         float64   `json:"cores"`
	Replicas      int32     `json:"replicas"`
	CapacityCores *float64  `json:"capacity,omitempty"`
}

// ForecastPoint is a predicted average over [At, Until).
type ForecastPoint struct {
	At    time.Time `json:"at"`
	Until time.Time `json:"until"`
	Value float64   `json:"value"`
}

// ModelSeries is the CPU history one model keeps and its newest forecast in cores.
type ModelSeries struct {
	Model    string          `json:"model"`
	History  []Sample        `json:"history"`
	Forecast []ForecastPoint `json:"forecast,omitempty"`
}

// DemandWindow is one closed request window of a request-demand model. Rates
// are per second, CPU is in cores and latency in seconds; nil was not measured.
// Needed is the CPU the window's requests needed: measured CPU times arrivals
// per completion, which exceeds measured CPU while pods fall behind.
type DemandWindow struct {
	At          time.Time `json:"at"`
	Until       time.Time `json:"until"`
	Arrivals    *float64  `json:"arrivals,omitempty"`
	Completions *float64  `json:"completions,omitempty"`
	Errors      *float64  `json:"errors,omitempty"`
	LatencyP95  *float64  `json:"latencyP95,omitempty"`
	Cores       *float64  `json:"cores,omitempty"`
	Needed      *float64  `json:"needed,omitempty"`
	Ready       *float64  `json:"ready,omitempty"`
}

// DemandSeries is a request-demand model's recent windows and newest forecast,
// in requests per second for target arrivals and in cores for reconstructedCPU.
type DemandSeries struct {
	Model    string          `json:"model"`
	Target   string          `json:"target"`
	Windows  []DemandWindow  `json:"windows"`
	Forecast []ForecastPoint `json:"forecast,omitempty"`
	IssuedAt *time.Time      `json:"issuedAt,omitempty"`
}

// Marker is a moment the charts draw a line at.
type Marker struct {
	At    time.Time `json:"at"`
	Label string    `json:"label"`
}

// Detail is everything the page of one HPAPlus shows.
type Detail struct {
	Summary
	CPUCores *float64  `json:"cpuCores,omitempty"`
	Capacity *Capacity `json:"capacity,omitempty"`
	// LimitCoresPerPod is the CPU limit of one pod of the scale target, summed
	// over its containers; absent when a container has no CPU limit.
	LimitCoresPerPod *float64            `json:"limitCoresPerPod,omitempty"`
	Decision         *api.DecisionStatus `json:"decision,omitempty"`
	Conditions       []metav1.Condition  `json:"conditions,omitempty"`
	Markers          []Marker            `json:"markers"`
	Series           []ModelSeries       `json:"series"`
	Demand           []DemandSeries      `json:"demand,omitempty"`
	HistoryError     string              `json:"historyError,omitempty"`
	Now              time.Time           `json:"now"`
}

// Reader loads views. It never writes.
type Reader struct {
	Client client.Reader
	Now    func() time.Time
}

func (r *Reader) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Overview lists every HPAPlus in a namespace, or in all namespaces when empty.
// It reads only the HPAPlus objects, so it stays cheap for many of them.
func (r *Reader) Overview(ctx context.Context, namespace string) ([]Summary, error) {
	list := &api.HPAPlusList{}
	if err := r.Client.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, r.summary(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Trend returns the newest stored CPU and replica samples of one HPAPlus.
func (r *Reader) Trend(ctx context.Context, namespace, name string) ([]Sample, error) {
	item := &api.HPAPlus{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, item); err != nil {
		return nil, err
	}
	data, err := r.history(ctx, item)
	if err != nil {
		return nil, err
	}
	var newest []Sample
	for _, series := range seriesOf(data) {
		h := series.History
		if len(h) > 0 && (len(newest) == 0 || h[len(h)-1].Time.After(newest[len(newest)-1].Time)) {
			newest = h
		}
	}
	if len(newest) > trendSamples {
		newest = newest[len(newest)-trendSamples:]
	}
	if newest == nil {
		newest = []Sample{}
	}
	return newest, nil
}

// Detail loads one HPAPlus with its target, history, forecasts and request windows.
func (r *Reader) Detail(ctx context.Context, namespace, name string) (*Detail, error) {
	item := &api.HPAPlus{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, item); err != nil {
		return nil, err
	}
	detail := &Detail{Summary: r.summary(item), Decision: item.Status.LastDecision, Conditions: item.Status.Conditions, Markers: markers(item),
		Series: []ModelSeries{}, Now: r.now()}
	ref := item.Spec.ScaleTargetRef
	if target, err := r.target(ctx, item.Namespace, ref.APIVersion, ref.Kind, ref.Name); err == nil {
		detail.Ready, detail.Replicas, detail.LimitCoresPerPod = target.Ready, target.Replicas, target.LimitCoresPerPod
	}
	data, err := r.history(ctx, item)
	if err != nil {
		detail.HistoryError = err.Error()
	} else {
		detail.Series = seriesOf(data)
		var latest *Sample
		for _, series := range detail.Series {
			if h := series.History; len(h) > 0 && (latest == nil || h[len(h)-1].Time.After(latest.Time)) {
				latest = &h[len(h)-1]
			}
		}
		if latest != nil {
			detail.CPUCores = &latest.Cores
		}
		detail.Capacity = capacityOf(data)
	}
	detail.Demand = r.demand(ctx, item)
	return detail, nil
}

func (r *Reader) summary(item *api.HPAPlus) Summary {
	s := Summary{Namespace: item.Namespace, Name: item.Name, Mode: api.ModeControl, Replicas: item.Status.CurrentReplicas,
		Min: 1, Max: item.Spec.MaxReplicas, Models: []ModelSummary{}}
	if item.Spec.Mode != nil && *item.Spec.Mode != "" {
		s.Mode = *item.Spec.Mode
	}
	if item.Spec.MinReplicas != nil {
		s.Min = *item.Spec.MinReplicas
	}
	ref := item.Spec.ScaleTargetRef
	s.Target = ref.Kind + "/" + ref.Name
	if d := item.Status.LastDecision; d != nil {
		desired := d.DesiredReplicas
		evaluated := d.EvaluatedAt.Time
		s.Desired, s.ScaleWrite, s.EvaluatedAt = &desired, d.ScaleWrite, &evaluated
	}
	types := map[string]string{}
	for _, m := range item.Spec.Models {
		types[m.Name] = m.Type
	}
	for _, m := range item.Status.ModelStatuses {
		ms := ModelSummary{Name: m.Name, Type: types[m.Name], State: m.State, Samples: m.HistorySamples, Required: m.RequiredSamples,
			ForecastReplicas: m.ForecastReplicas, LastRunOutcome: m.LastRunOutcome}
		if m.LastSuccessTime != nil {
			t := m.LastSuccessTime.Time
			ms.LastSuccess = &t
		}
		if m.ExpiresAt != nil {
			t := m.ExpiresAt.Time
			ms.ExpiresAt = &t
		}
		if rd := m.RequestDemand; rd != nil {
			ms.Source, ms.DecidedBy, ms.FloorReplicas, ms.ForecastOnly = rd.Source, rd.DecidedBy, rd.FloorReplicas, rd.ForecastOnlyReplicas
			ms.MeasuredCores, ms.CPUPerRequest, ms.FallbackReason = rd.MeasuredCores, rd.CPUSecondsPerRequest, rd.FallbackReason
			if s.DecidedBy == "" {
				s.DecidedBy = rd.DecidedBy
			}
		}
		s.Models = append(s.Models, ms)
	}
	s.Health, s.Reason = health(item, s, r.now())
	return s
}

// health is a plain reading of the latest reconciliation: no longer reconciled,
// something to look at, still learning, or working. An old decision comes
// first because every other field is as old as it.
func health(item *api.HPAPlus, s Summary, now time.Time) (string, string) {
	if s.EvaluatedAt != nil && now.Sub(*s.EvaluatedAt) > IdleAfter {
		return "idle", "No recent decision. The operator may be stopped, or it cannot reconcile this HPAPlus; its logs say which."
	}
	for _, c := range item.Status.Conditions {
		if c.Status != metav1.ConditionFalse {
			continue
		}
		switch c.Type {
		case "MetricsAvailable":
			if c.Reason == "PartialMetrics" && now.Sub(c.LastTransitionTime.Time) < PartialMetricsGrace {
				return "partial", c.Message
			}
			return "attention", c.Message
		case "ConfigurationValid", "SamplingActive", "HistoryCapacityAvailable":
			return "attention", c.Message
		case "ScalingActive":
			if c.Reason != "ObserveOnly" {
				return "attention", c.Message
			}
		}
	}
	if s.EvaluatedAt == nil {
		return "waiting", "No decision yet."
	}
	for _, m := range s.Models {
		switch m.State {
		case "Backoff", "Failed", "InvalidHistory", "Unavailable", "Expired":
			return "attention", fmt.Sprintf("Model %s is %s.", m.Name, m.State)
		case "Waiting", "WarmingUp":
			return "learning", fmt.Sprintf("Model %s has %d of the %d samples it needs.", m.Name, m.Samples, m.Required)
		}
		if m.FallbackReason != "" {
			return "attention", fmt.Sprintf("Model %s: %s", m.Name, m.FallbackReason)
		}
	}
	for _, c := range item.Status.Conditions {
		if c.Type == "PredictionsAvailable" && c.Status == metav1.ConditionFalse {
			return "attention", c.Message
		}
	}
	if s.ScaleWrite == "rejected" {
		return "attention", "The last change to the pod count was rejected."
	}
	return "healthy", ""
}

// markers finds when forecasts became usable and when the mode last changed.
// HPA+ keeps no history of its own specification, and its ScalingActive
// condition restarts after any failed reconciliation, so the mode change is
// read from the field manager that last set spec.mode.
func markers(item *api.HPAPlus) []Marker {
	out := []Marker{}
	for _, c := range item.Status.Conditions {
		if c.Type == "PredictionsAvailable" && c.Status == metav1.ConditionTrue {
			out = append(out, Marker{At: c.LastTransitionTime.Time, Label: "Forecasts ready"})
		}
	}
	var changed *metav1.Time
	onlyMode := false
	for _, entry := range item.ManagedFields {
		if entry.Subresource != "" || entry.Time == nil || entry.FieldsV1 == nil {
			continue
		}
		var fields struct {
			Spec map[string]json.RawMessage `json:"f:spec"`
		}
		if json.Unmarshal(entry.FieldsV1.Raw, &fields) != nil {
			continue
		}
		if _, ok := fields.Spec["f:mode"]; !ok || (changed != nil && !entry.Time.After(changed.Time)) {
			continue
		}
		changed, onlyMode = entry.Time, true
		for key := range fields.Spec {
			onlyMode = onlyMode && (key == "f:mode" || key == ".")
		}
	}
	// A mode set when the object was created is where its history begins anyway.
	if changed == nil || changed.Sub(item.CreationTimestamp.Time) < 5*time.Second {
		return out
	}
	label := "Settings changed"
	if onlyMode {
		mode := api.ModeControl
		if item.Spec.Mode != nil && *item.Spec.Mode != "" {
			mode = *item.Spec.Mode
		}
		label = map[string]string{api.ModeControl: "Took control", api.ModeAssist: "Started assisting", api.ModeObserve: "Watching only"}[mode]
	}
	return append(out, Marker{At: changed.Time, Label: label})
}

// targetInfo is what the UI reads from the scale target itself.
type targetInfo struct {
	Ready            *int32
	Replicas         int32
	LimitCoresPerPod *float64
}

// target reads the scale target's replicas and its pod template's CPU limit.
// Common workload kinds report readiness as status.readyReplicas; others only
// report spec replicas.
func (r *Reader) target(ctx context.Context, namespace, apiVersion, kind, name string) (targetInfo, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return targetInfo{}, err
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gv.WithKind(kind))
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		return targetInfo{}, err
	}
	replicas, _, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas")
	info := targetInfo{Replicas: int32(replicas)}
	if value, found, _ := unstructured.NestedInt64(obj.Object, "status", "readyReplicas"); found {
		v := int32(value)
		info.Ready = &v
	} else if _, ok := obj.Object["status"]; ok {
		zero := int32(0)
		info.Ready = &zero
	}
	info.LimitCoresPerPod = podCPULimit(obj.Object)
	return info, nil
}

// podCPULimit sums the CPU limits of a pod template's containers; a container
// without a limit leaves the pod unlimited.
func podCPULimit(object map[string]interface{}) *float64 {
	containers, found, _ := unstructured.NestedSlice(object, "spec", "template", "spec", "containers")
	if !found || len(containers) == 0 {
		return nil
	}
	total := 0.
	for _, c := range containers {
		container, ok := c.(map[string]interface{})
		if !ok {
			return nil
		}
		text, found, _ := unstructured.NestedString(container, "resources", "limits", "cpu")
		if !found {
			return nil
		}
		quantity, err := resource.ParseQuantity(text)
		if err != nil {
			return nil
		}
		total += float64(quantity.MilliValue()) / 1000
	}
	return &total
}

// history reads the stored model histories without the operator's write paths:
// the root ConfigMap holds either the data or a manifest of immutable chunks.
func (r *Reader) history(ctx context.Context, owner *api.HPAPlus) (*api.HPAPlusData, error) {
	root := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: "hpa-plus-" + owner.Name + "-data"}, root); err != nil {
		if apierrors.IsNotFound(err) {
			return &api.HPAPlusData{ModelHistories: map[string]api.ModelHistory{}}, nil
		}
		return nil, err
	}
	if !history.OwnedConfigMap(root, owner) {
		return nil, fmt.Errorf("history ConfigMap is owned by another resource")
	}
	payload := root.Data["data"]
	if payload == "" {
		return &api.HPAPlusData{ModelHistories: map[string]api.ModelHistory{}}, nil
	}
	var header struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal([]byte(payload), &header); err != nil {
		return nil, err
	}
	if header.SchemaVersion != 2 {
		return decode(payload, history.MaxDataBytes)
	}
	manifest, err := history.DecodeManifest(payload, root)
	if err != nil {
		return nil, err
	}
	state := make([]byte, 0)
	for _, ref := range manifest.Chunks {
		chunk := &corev1.ConfigMap{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: root.Namespace, Name: ref.Name}, chunk); err != nil {
			return nil, err
		}
		bytes := chunk.BinaryData["state"]
		digest := sha256.Sum256(bytes)
		if !history.OwnedConfigMap(chunk, owner) || string(chunk.UID) != ref.UID || hex.EncodeToString(digest[:]) != ref.Digest {
			return nil, fmt.Errorf("history chunk changed while reading")
		}
		state = append(state, bytes...)
	}
	return decode(string(state), history.MaxAllocatedBytes)
}

// decode reads stored history for display. The operator's decoder rejects
// unknown fields because it writes the data back; the UI never writes, so it
// also shows history written by another HPA+ version and skips what it does
// not know.
func decode(payload string, limit int) (*api.HPAPlusData, error) {
	data, err := history.DecodeBounded(payload, limit)
	if err == nil {
		return data, nil
	}
	lenient := &api.HPAPlusData{}
	if len(payload) > limit || json.Unmarshal([]byte(payload), lenient) != nil {
		return nil, err
	}
	if lenient.ModelHistories == nil {
		lenient.ModelHistories = map[string]api.ModelHistory{}
	}
	return lenient, nil
}

func seriesOf(data *api.HPAPlusData) []ModelSeries {
	names := make([]string, 0, len(data.ModelHistories))
	for name := range data.ModelHistories {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ModelSeries, 0, len(names))
	for _, name := range names {
		h := data.ModelHistories[name]
		series := ModelSeries{Model: name, History: samplesOf(h.ReplicaHistory)}
		if c := h.CachedPrediction; c != nil {
			for _, p := range c.Trajectory {
				at, e1 := time.Parse(time.RFC3339Nano, p.At)
				until, e2 := time.Parse(time.RFC3339Nano, p.Until)
				if e1 == nil && e2 == nil {
					series.Forecast = append(series.Forecast, ForecastPoint{At: at, Until: until, Value: float64(p.CPU) / 1000})
				}
			}
		}
		out = append(out, series)
	}
	return out
}

func samplesOf(rows []api.TimestampedSample) []Sample {
	out := make([]Sample, 0, len(rows))
	for _, row := range rows {
		if row.Time == nil || row.TotalCPUUsageMillicores == nil {
			continue
		}
		sample := Sample{Time: row.Time.Time, Cores: float64(*row.TotalCPUUsageMillicores) / 1000, Replicas: row.Replicas}
		if row.RequestPerPodMillicores != nil && row.TargetCPUUtilizationPercentage != nil {
			capacity := float64(row.Replicas) * float64(*row.RequestPerPodMillicores) / 1000 * float64(*row.TargetCPUUtilizationPercentage) / 100
			sample.CapacityCores = &capacity
		}
		out = append(out, sample)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

func capacityOf(data *api.HPAPlusData) *Capacity {
	var newest *api.TimestampedSample
	for _, h := range data.ModelHistories {
		for i := range h.ReplicaHistory {
			row := &h.ReplicaHistory[i]
			if row.Time != nil && row.RequestPerPodMillicores != nil && row.TargetCPUUtilizationPercentage != nil &&
				(newest == nil || row.Time.After(newest.Time.Time)) {
				newest = row
			}
		}
	}
	if newest == nil {
		return nil
	}
	return &Capacity{RequestCores: float64(*newest.RequestPerPodMillicores) / 1000, TargetPercent: *newest.TargetCPUUtilizationPercentage}
}

// demandState is the subset of a request-demand model's persisted state the UI shows.
type demandState struct {
	Observations []struct {
		At                time.Time `json:"at"`
		Until             time.Time `json:"until"`
		ArrivalsPerSecond *float64  `json:"arrivalsPerSecond"`
		Features          map[string]struct {
			Value *float64 `json:"value"`
		} `json:"features"`
	} `json:"observations"`
	Forecasts []struct {
		At    time.Time `json:"at"`
		Until time.Time `json:"until"`
		Value float64   `json:"value"`
	} `json:"forecasts"`
	ForecastIssuedAt time.Time `json:"forecastIssuedAt"`
}

func demandStateName(owner *api.HPAPlus, model string) string {
	raw, _ := json.Marshal([]string{string(owner.UID), model})
	sum := sha256.Sum256(raw)
	return "hpaplus-demand-" + hex.EncodeToString(sum[:])[:24]
}

// demand reads each request-demand model's state ConfigMap, if it exists.
func (r *Reader) demand(ctx context.Context, owner *api.HPAPlus) []DemandSeries {
	var out []DemandSeries
	for _, model := range owner.Spec.Models {
		if model.RequestDemand == nil {
			continue
		}
		cm := &corev1.ConfigMap{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: demandStateName(owner, model.Name)}, cm); err != nil {
			continue
		}
		if ref := metav1.GetControllerOf(cm); ref == nil || ref.UID != owner.UID {
			continue
		}
		state := demandState{}
		if raw := cm.Data["state"]; raw == "" || json.Unmarshal([]byte(raw), &state) != nil {
			continue
		}
		series := DemandSeries{Model: model.Name, Target: "arrivals", Windows: []DemandWindow{}}
		if model.RequestDemand.ForecastTarget != "" {
			series.Target = model.RequestDemand.ForecastTarget
		}
		if !state.ForecastIssuedAt.IsZero() {
			issued := state.ForecastIssuedAt
			series.IssuedAt = &issued
		}
		for _, o := range state.Observations {
			value := func(name string) *float64 {
				if f, ok := o.Features[name]; ok && f.Value != nil && !math.IsNaN(*f.Value) && !math.IsInf(*f.Value, 0) && *f.Value >= 0 {
					return f.Value
				}
				return nil
			}
			w := DemandWindow{At: o.At, Until: o.Until, Arrivals: value("traffic_arrivals"), Completions: value("traffic_completions"),
				Errors: value("traffic_errors"), LatencyP95: value("traffic_latency_p95"), Cores: value("cpu_usage"), Ready: value("ready_replicas")}
			if w.Arrivals == nil && o.ArrivalsPerSecond != nil {
				w.Arrivals = o.ArrivalsPerSecond
			}
			if w.Cores != nil && w.Arrivals != nil && w.Completions != nil && *w.Completions > 0 {
				needed := *w.Cores * *w.Arrivals / *w.Completions
				w.Needed = &needed
			}
			series.Windows = append(series.Windows, w)
		}
		for _, p := range state.Forecasts {
			series.Forecast = append(series.Forecast, ForecastPoint{At: p.At, Until: p.Until, Value: p.Value})
		}
		out = append(out, series)
	}
	return out
}
