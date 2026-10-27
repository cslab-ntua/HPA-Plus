package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/cgroup"
	"github.com/cslab-ntua/HPA-Plus/internal/decision"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	requestforecast "github.com/cslab-ntua/HPA-Plus/internal/prediction/requestdemand"
	"github.com/cslab-ntua/HPA-Plus/internal/workloaddemand"
	gathered "github.com/jthomperoo/k8shorizmetrics/v2/metrics"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

type demandIntegrationGatherer struct{ source string }

func TestRequestDemandDecisionEvidenceIsPreservedByCRDSchema(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "helm", "templates", "crd", "hpa.plus_hpapluses.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	jsonBytes, err := yaml.YAMLToJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &doc); err != nil {
		t.Fatal(err)
	}
	versions, found, err := unstructured.NestedSlice(doc, "spec", "versions")
	if err != nil || !found || len(versions) == 0 {
		t.Fatal("CRD versions missing")
	}
	version := versions[0].(map[string]interface{})
	prefix := []string{"schema", "openAPIV3Schema", "properties", "status", "properties"}
	primary, found, err := unstructured.NestedMap(version, append(append([]string{}, prefix...), "modelStatuses", "items", "properties", "requestDemand")...)
	if err != nil || !found {
		t.Fatal("primary requestDemand status schema missing")
	}
	evidence, found, err := unstructured.NestedMap(version, append(append([]string{}, prefix...), "lastDecision", "properties", "evidence", "properties", "models", "items", "properties", "requestDemand")...)
	if err != nil || !found || !reflect.DeepEqual(primary, evidence) {
		t.Fatal("decision evidence prunes/differs from requestDemand status")
	}
	outcomes, found, err := unstructured.NestedMap(version, "schema", "openAPIV3Schema", "properties", "spec", "properties", "models", "items", "properties", "requestDemand", "properties", "traffic", "properties", "outcomesMetric")
	if err != nil || !found || outcomes["type"] != "string" {
		t.Fatal("CRD prunes the optional all-outcomes counter")
	}
	enums, found, err := unstructured.NestedSlice(version, append(append([]string{}, prefix...), "lastDecision", "properties", "candidateMode", "enum")...)
	if err != nil || !found {
		t.Fatal("decision mode schema missing")
	}
	for _, mode := range enums {
		if mode == "hold" {
			return
		}
	}
	t.Fatal("CRD rejects explicit prediction failure hold decisions")
}

type demandIntegrationRunner func(string, string, int) (string, error)

func (f demandIntegrationRunner) RunAlgorithmWithValue(script, payload string, timeout int) (string, error) {
	return f(script, payload, timeout)
}

func (g *demandIntegrationGatherer) CPUHistorySourceIdentity() string { return g.source }
func (*demandIntegrationGatherer) Gather(context.Context, []autoscalingv2.MetricSpec, string, labels.Selector, time.Duration, time.Duration) ([]*gathered.Metric, error) {
	return nil, fmt.Errorf("test must not gather reactive metrics")
}
func (*demandIntegrationGatherer) GatherCgroupResources(context.Context, string, labels.Selector, time.Time, time.Time) (*cgroup.WorkloadObservation, error) {
	return nil, fmt.Errorf("test must not acquire a new accounting interval")
}
func (*demandIntegrationGatherer) GatherCgroupPodResources(context.Context, string, string, string, time.Time, time.Time) (*cgroup.WorkloadObservation, error) {
	return nil, fmt.Errorf("test must not acquire a new accounting interval")
}

func demandIntegrationFixture(t *testing.T) (*HPAPlusReconciler, *api.HPAPlus, *autoscalingv1.Scale, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 20, 17, 0, 4, 0, time.UTC)
	no := false
	age := 120000
	target := int32(60)
	one := int32(1)
	owner := &api.HPAPlus{TypeMeta: metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "HPAPlus"}, ObjectMeta: metav1.ObjectMeta{Name: "predictive", Namespace: "a", UID: types.UID("owner-a"), Generation: 1}, Spec: api.HPAPlusSpec{IncludeHPA: &no, PredictionFailurePolicy: api.PredictionFailureHoldCurrent, MinReplicas: &one, MaxReplicas: 8, ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "app"}, Metrics: []autoscalingv2.MetricSpec{{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceCPU, Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: &target}}}}, Models: []api.Model{{Type: api.TypeXGBoost, Name: "xgb", XGBoost: &api.XGBoost{HistorySize: 60, Lags: 12, LookAhead: 90000, ForecastStrategy: "direct", SeasonalFeatures: true}, PredictivePolicy: &api.PredictivePolicy{}, PredictionMaxAge: &age, RequestDemand: &api.RequestDemandConfig{Traffic: api.TrafficProviderConfig{Provider: "prometheus", ArrivalsMetric: "started_total", CompletionsMetric: "finished_total", ArrivalSemantics: "started", CompletionSemantics: "finished", RetrySemantics: "attempts"}}}}}}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "a", UID: types.UID("workload-a")}, Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "a"}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "a"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:v1", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}}}}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app-pod", Namespace: "a", UID: types.UID("pod-a"), Labels: map[string]string{"app": "a"}}, Spec: deployment.Spec.Template.Spec, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, api.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, deployment, pod).Build()
	r := &HPAPlusReconciler{Client: c, APIReader: c, Scheme: scheme, Now: func() time.Time { return now }, Gatherer: &demandIntegrationGatherer{source: "metrics-server-v1"}}
	scale := &autoscalingv1.Scale{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "a", UID: types.UID("workload-a")}, Spec: autoscalingv1.ScaleSpec{Replicas: 3}, Status: autoscalingv1.ScaleStatus{Replicas: 3, Selector: "app=a"}}
	return r, owner, scale, &now
}

func TestColdLearnerPersistsEpochBeforeFirstAccountingWindow(t *testing.T) {
	r, owner, _, now := demandIntegrationFixture(t)
	ctx := context.Background()
	model := &owner.Spec.Models[0]
	_, first, err := r.loadDemandState(ctx, owner, model, "cold-identity")
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(45 * time.Second)
	cm, next, err := r.loadDemandState(ctx, owner, model, "cold-identity")
	if err != nil {
		t.Fatal(err)
	}
	if !next.CreatedAt.Equal(first.CreatedAt) || cm.Data["state"] == "" {
		t.Fatalf("cold state epoch keeps moving and can never close its first window: first=%v next=%v", first.CreatedAt, next.CreatedAt)
	}
	if now.Sub(next.CreatedAt) != 45*time.Second {
		t.Fatal("learner did not age while awaiting its first real interval")
	}
}

func TestDemandScopeIncludesPodsDeletedBetweenReconciliations(t *testing.T) {
	scope := workloaddemand.Scope{Namespace: "a", Pods: []workloaddemand.PodIdentity{{Name: "live", UID: "live-uid"}}}
	window := &cgroup.WorkloadObservation{Pods: []cgroup.Pod{
		{Namespace: "a", Name: "live", UID: "live-uid"},
		{Namespace: "a", Name: "already-deleted", UID: "old-uid"},
		{Namespace: "b", Name: "other-namespace", UID: "other-uid"},
	}}
	actual := demandWindowScope(scope, window)
	if len(actual.Pods) != 2 || actual.Pods[1].UID != "old-uid" {
		t.Fatalf("historical pod traffic was omitted or crossed namespace: %+v", actual)
	}
}

func TestCPUBackendHistoryFencePersistsAndIsIdempotent(t *testing.T) {
	r, owner, scale, now := demandIntegrationFixture(t)
	ctx := context.Background()
	data, err := r.ensureTargetHistory(ctx, owner, scale)
	if err != nil {
		t.Fatal(err)
	}
	if data.CPUHistorySource != "metrics-server-v1" {
		t.Fatalf("source not bound %+v", data)
	}
	usage := int64(300)
	pending := &api.ScaleDecision{ID: "pending", OwnerUID: string(owner.UID), Target: owner.Spec.ScaleTargetRef, TargetUID: string(scale.UID), ResourceVersion: "77", PreviousDesired: 3, Desired: 4, Observed: 3, Time: metav1.NewTime(*now)}
	if err := r.mutateConfigMapData(ctx, owner, func(d *api.HPAPlusData) (bool, error) {
		d.ModelHistories["xgb"] = api.ModelHistory{Type: api.TypeXGBoost, HistoryEpoch: 7, ReplicaHistory: []api.TimestampedSample{{Time: &metav1.Time{Time: now.Add(-time.Minute)}, TotalCPUUsageMillicores: &usage}}}
		d.PendingScale = pending
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	before := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, historyRootKey(owner), before); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ensureTargetHistory(ctx, owner, scale); err != nil {
		t.Fatal(err)
	}
	after := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, historyRootKey(owner), after); err != nil {
		t.Fatal(err)
	}
	if before.ResourceVersion != after.ResourceVersion {
		t.Fatal("unchanged source rewrote history")
	}
	r.Gatherer = &demandIntegrationGatherer{source: "hpaplus-cgroup-v1/30s"}
	data, err = r.ensureTargetHistory(ctx, owner, scale)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.ModelHistories) != 0 || data.PendingScale == nil || data.PendingScale.ID != "pending" || data.CPUHistorySource != "hpaplus-cgroup-v1/30s" {
		t.Fatalf("backend fence reset wrong state %+v", data)
	}
	restarted := *r
	data, err = restarted.ensureTargetHistory(ctx, owner, scale)
	if err != nil || data.PendingScale == nil || len(data.ModelHistories) != 0 {
		t.Fatalf("restart changed fence result %+v %v", data, err)
	}
}

func TestRequestDemandStateNamespaceOwnerAndRestartIsolation(t *testing.T) {
	r, owner, _, now := demandIntegrationFixture(t)
	ctx := context.Background()
	model := &owner.Spec.Models[0]
	cm, state, err := r.loadDemandState(ctx, owner, model, "identity-a")
	if err != nil {
		t.Fatal(err)
	}
	state.Forecasts = []requestforecast.Point{{At: now.Add(-4 * time.Second), Until: now.Add(26 * time.Second), Value: 120}}
	state.LastSlot = now.Add(-4 * time.Second)
	if err := r.saveDemandState(ctx, cm, state); err != nil {
		t.Fatal(err)
	}
	restarted := *r
	_, restored, err := restarted.loadDemandState(ctx, owner, model, "identity-a")
	if err != nil || len(restored.Forecasts) != 1 || restored.Forecasts[0].Value != 120 {
		t.Fatalf("restart lost issued interval %+v %v", restored, err)
	}
	other := owner.DeepCopy()
	other.Namespace = "b"
	other.ResourceVersion = ""
	if err := r.Client.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	_, separate, err := r.loadDemandState(ctx, other, model, "identity-b")
	if err != nil || len(separate.Forecasts) != 0 || separate.Identity != "identity-b" {
		t.Fatalf("namespace shared learned state %+v %v", separate, err)
	}
	_, reset, err := r.loadDemandState(ctx, owner, model, "new-template-identity")
	if err != nil || len(reset.Forecasts) != 0 || reset.Identity != "new-template-identity" {
		t.Fatalf("configuration reused old forecast %+v %v", reset, err)
	}
	latest := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(cm), latest); err != nil {
		t.Fatal(err)
	}
	latest.OwnerReferences[0].UID = types.UID("unrelated-owner")
	if err := r.Client.Update(ctx, latest); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.loadDemandState(ctx, owner, model, "identity-a"); err == nil {
		t.Fatal("took over unrelated state")
	}
}

type demandFinalRecheckReader struct {
	client.Reader
	key   client.ObjectKey
	after func(client.Object)
}

func (r *demandFinalRecheckReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if err := r.Reader.Get(ctx, key, object, options...); err != nil {
		return err
	}
	if key == r.key && r.after != nil {
		r.after(object)
	}
	return nil
}

func TestRestartedRequestDemandUsesQualifiedLiveInterval(t *testing.T) {
	r, owner, scale, now := demandIntegrationFixture(t)
	r.TrafficPrometheusURL = "http://unused.invalid"
	ctx := context.Background()
	model := &owner.Spec.Models[0]
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
	state.CreatedAt = now.Add(-time.Hour)
	state.LastSlot = now.Add(-4 * time.Second)
	state.LastFitSlot = state.LastSlot.Add(-30 * time.Second)
	state.ForecastConsumedAt = state.LastSlot
	state.Forecasts = []requestforecast.Point{{At: state.LastSlot, Until: state.LastSlot.Add(30 * time.Second), Value: 120}}
	state.Estimator = workloaddemand.NewEstimator(identity).Snapshot()
	state.Estimator.Last = workloaddemand.Estimate{State: "qualified", CPUSecondsPerRequest: .02, BackgroundCPUPerPod: .1, QualifiedWindows: 25, Version: 6, LastObservationAt: state.LastSlot, FreshUntil: state.LastSlot.Add(2 * time.Minute)}
	for i := 0; i < 60; i++ {
		at := state.LastSlot.Add(time.Duration(i-60) * 30 * time.Second)
		value := float64(20 + i%7)
		state.Observations = append(state.Observations, requestforecast.Observation{At: at, Until: at.Add(30 * time.Second), ReceivedAt: at.Add(34 * time.Second), ArrivalsPerSecond: &value})
	}
	workerCalls := 0
	r.WorkloadRunner = demandIntegrationRunner(func(script, payload string, timeout int) (string, error) {
		workerCalls++
		var request struct {
			RequestedAt, FirstForecastAt time.Time
			Config                       requestforecast.Config
			Observations                 []requestforecast.Observation
		}
		if err := json.Unmarshal([]byte(payload), &request); err != nil {
			return "", err
		}
		if script != requestforecast.Script || len(request.Observations) != 60 || request.Config.HistorySize != 60 || request.Config.Lags != 12 || request.Config.HorizonSteps != 3 {
			t.Fatalf("wrong request worker contract: %s %+v", script, request.Config)
		}
		if !request.FirstForecastAt.Equal(state.LastSlot.Add(30 * time.Second)) {
			t.Fatalf("request forecast skipped its first unstarted interval: %v", request.FirstForecastAt)
		}
		result := requestforecast.Result{Protocol: requestforecast.Protocol, Unit: requestforecast.Unit, GeneratedAt: request.RequestedAt, Status: "ready", TrainedThrough: state.LastSlot}
		for i := 0; i < 3; i++ {
			at := request.FirstForecastAt.Add(time.Duration(i) * 30 * time.Second)
			result.Forecasts = append(result.Forecasts, requestforecast.Point{At: at, Until: at.Add(30 * time.Second), Value: 10})
		}
		body, err := json.Marshal(result)
		return string(body), err
	})
	if err := r.saveDemandState(ctx, cm, state); err != nil {
		t.Fatal(err)
	}
	request := int64(1000)
	target := int32(60)
	snapshot := cpuMetricSnapshot{RequestPerPodMillicores: &request, TargetUtilizationPercentage: &target}
	report := &api.ModelStatus{}
	restarted := *r
	n, deadline, usable := restarted.processRequestDemand(ctx, owner, model, scale, snapshot, report, nil)
	if !usable || n != 5 || !deadline.Equal(state.Forecasts[0].Until) || report.RequestDemand.Source != "request-demand" {
		t.Fatalf("issued request forecast not restored: n=%d usable=%v status=%+v deadline=%v", n, usable, report, deadline)
	}
	if workerCalls != 1 {
		t.Fatalf("request worker not invoked once: %d", workerCalls)
	}
	// Exercise the model orchestrator and final decision too: a qualified request
	// forecast does not need sixty CPU fallback samples to supply its candidate.
	data := &api.HPAPlusData{TargetIdentity: scaleTargetIdentity(owner, scale), CPUHistorySource: "hpaplus-cgroup-v1/30s", ModelHistories: map[string]api.ModelHistory{"xgb": {Type: api.TypeXGBoost, HistoryEpoch: 1}}}
	payload, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	historyCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: historyRootKey(owner).Name, Namespace: owner.Namespace, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(owner, api.GroupVersion.WithKind("HPAPlus"))}}, Data: map[string]string{configMapDataKey: string(payload)}}
	if err := r.Client.Create(ctx, historyCM); err != nil {
		t.Fatal(err)
	}
	snapshot.TargetIdentity = data.TargetIdentity
	predictions, _, statuses := restarted.processModels(ctx, owner, data, *now, scale.Spec.Replicas, 8, snapshot, scale)
	if len(predictions) != 1 || predictions[0] != 5 || statuses[0].RequestDemand.Source != "request-demand" {
		t.Fatalf("request candidate lost by CPU fallback orchestration %+v %+v", predictions, statuses)
	}
	if workerCalls != 1 {
		t.Fatal("controller replayed fitted slot after restart")
	}
	plan, err := decision.Plan(decision.Input{Spec: owner.Spec, Now: *now, Requested: 3, Baseline: 8, Predictions: predictions})
	if err != nil || plan.Desired != 5 || plan.BaselineIncluded {
		t.Fatalf("reactive leaked into learned request decision %+v %v", plan, err)
	}
	// A request candidate can expire while final history fencing is read.
	// Its source must describe the remaining decision, not the earlier fit.
	for _, fence := range []string{"expiry", "epoch"} {
		t.Run("final-"+fence, func(t *testing.T) {
			original := *now
			checked := restarted
			checked.APIReader = &demandFinalRecheckReader{Reader: restarted.Client, key: historyRootKey(owner), after: func(object client.Object) {
				if fence == "expiry" {
					*now = deadline
					return
				}
				config := object.(*corev1.ConfigMap)
				var changed api.HPAPlusData
				if err := json.Unmarshal([]byte(config.Data[configMapDataKey]), &changed); err != nil {
					t.Fatal(err)
				}
				history := changed.ModelHistories[model.Name]
				history.HistoryEpoch++
				changed.ModelHistories[model.Name] = history
				payload, err := json.Marshal(changed)
				if err != nil {
					t.Fatal(err)
				}
				config.Data[configMapDataKey] = string(payload)
			}}
			predictions, candidates, reports := checked.processModels(ctx, owner, data, *now, scale.Spec.Replicas, 8, snapshot, scale)
			if len(predictions) != 0 || len(candidates) != 0 || reports[0].RequestDemand.Source != "hold" {
				t.Fatalf("discarded candidate remained active: %+v %+v %+v", predictions, candidates, reports)
			}
			*now = original
		})
	}
	*now = now.Add(3 * time.Minute)
	latestCM, staleState, err := restarted.loadDemandState(ctx, owner, model, identity)
	if err != nil {
		t.Fatal(err)
	}
	staleState.LastSlot = now.Add(-4 * time.Second)
	staleState.LastFitSlot = staleState.LastSlot
	if err := restarted.saveDemandState(ctx, latestCM, staleState); err != nil {
		t.Fatal(err)
	}
	report = &api.ModelStatus{}
	_, _, usable = restarted.processRequestDemand(ctx, owner, model, scale, snapshot, report, nil)
	if usable || report.RequestDemand.Source != "hold" {
		t.Fatalf("stale persisted estimate remained active: %+v", report)
	}
	// A different telemetry origin cannot reuse costs or arrival histories,
	// even when metric names and workload identities are unchanged.
	restarted.TrafficPrometheusURL = "http://different-prometheus.invalid"
	report = &api.ModelStatus{}
	_, _, usable = restarted.processRequestDemand(ctx, owner, model, scale, snapshot, report, nil)
	if usable || report.RequestDemand.State != "learning" {
		t.Fatalf("new telemetry origin reused old learning: %+v", report)
	}
	if err := restarted.Client.Get(ctx, client.ObjectKeyFromObject(cm), latestCM); err != nil {
		t.Fatal(err)
	}
	var reset demandState
	if err := json.Unmarshal([]byte(latestCM.Data["state"]), &reset); err != nil {
		t.Fatal(err)
	}
	if reset.Identity == identity || len(reset.Observations) != 0 || len(reset.Forecasts) != 0 || len(reset.Estimator.Windows) != 0 {
		t.Fatal("telemetry origin change retained incompatible learned state")
	}
}

func TestRequestDemandCPUFallbackThenHoldNeverUsesReactive(t *testing.T) {
	r, owner, scale, now := demandIntegrationFixture(t)
	ctx := context.Background()
	model := owner.Spec.Models[0]
	identity := scaleTargetIdentity(owner, scale)
	rows := make([]api.TimestampedSample, 60)
	usage := int64(1200)
	request := int64(1000)
	target := int32(60)
	for i := range rows {
		rows[i] = api.TimestampedSample{Time: &metav1.Time{Time: now.Add(time.Duration(i-60) * 30 * time.Second)}, TotalCPUUsageMillicores: &usage, RequestPerPodMillicores: &request, TargetCPUUtilizationPercentage: &target}
	}
	consumed := rows[len(rows)-1].Time.Time
	model.SessionID = modelHistorySessionID(owner, model.Name, 1, identity)
	model.CPURequestPerPodMillicores = request
	model.TargetCPUUtilizationPercentage = target
	fingerprint, err := predictionFingerprint(owner, &model)
	if err != nil {
		t.Fatal(err)
	}
	points := []api.CPUForecastPoint{}
	for i := 0; i < 3; i++ {
		at := consumed.Add(time.Duration(i+1) * 30 * time.Second)
		points = append(points, api.CPUForecastPoint{At: at.Format(time.RFC3339Nano), Until: at.Add(30 * time.Second).Format(time.RFC3339Nano), CPU: 1200})
	}
	cached := makeCachedPrediction(owner, &model, prediction.Result{Replicas: 2, ConsumedUntil: &consumed, Trajectory: points}, fingerprint, now.Add(-time.Second), rows)
	data := &api.HPAPlusData{TargetIdentity: identity, CPUHistorySource: "metrics-server-v1", ModelHistories: map[string]api.ModelHistory{"xgb": {Type: api.TypeXGBoost, HistoryEpoch: 1, ReplicaHistory: rows, LastModelRunTime: &metav1.Time{Time: consumed}, CachedPrediction: cached}}}
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: historyRootKey(owner).Name, Namespace: owner.Namespace, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(owner, api.GroupVersion.WithKind("HPAPlus"))}}, Data: map[string]string{configMapDataKey: string(encoded)}}
	if err := r.Client.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	snapshot := cpuMetricSnapshot{TargetIdentity: identity, RequestPerPodMillicores: &request, TargetUtilizationPercentage: &target}
	for _, restart := range []bool{false, true} {
		controller := r
		if restart {
			copy := *r
			controller = &copy
		}
		predictions, _, statuses := controller.processModels(ctx, owner, data, *now, scale.Spec.Replicas, 8, snapshot, scale)
		if len(predictions) != 1 || predictions[0] != 2 || len(statuses) != 1 || statuses[0].RequestDemand.Source != "cpu-forecast" {
			t.Fatalf("restart=%v CPU fallback lost %+v %+v", restart, predictions, statuses)
		}
		plan, err := decision.Plan(decision.Input{Spec: owner.Spec, Now: *now, Requested: 3, Baseline: 8, Predictions: predictions})
		if err != nil || plan.Desired != 2 || plan.BaselineIncluded {
			t.Fatalf("reactive leaked into CPU fallback %+v %v", plan, err)
		}
	}
	*now = now.Add(3 * time.Minute)
	predictions, _, statuses := r.processModels(ctx, owner, data, *now, scale.Spec.Replicas, 8, snapshot, scale)
	if len(predictions) != 0 || statuses[0].RequestDemand.Source != "hold" {
		t.Fatalf("expired CPU forecast reused %+v %+v", predictions, statuses)
	}
	plan, err := decision.Plan(decision.Input{Spec: owner.Spec, Now: *now, Requested: 3, Baseline: 8, Predictions: predictions})
	if err != nil || plan.Desired != 3 || plan.BaselineIncluded || plan.Mode != "hold" {
		t.Fatalf("reactive leaked into hold %+v %v", plan, err)
	}
}

func TestRequestDemandPublishesLaterIntervalsAfterSlowFitAcrossBoundary(t *testing.T) {
	r, owner, scale, now := demandIntegrationFixture(t)
	*now = now.Add(24 * time.Second) // Invocation at :28; a six-second fit returns at :34.
	r.TrafficPrometheusURL = "http://unused.invalid"
	ctx := context.Background()
	model := &owner.Spec.Models[0]
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
	slot := now.Truncate(30 * time.Second)
	state.CreatedAt = slot.Add(-time.Hour)
	state.LastSlot = slot
	state.LastFitSlot = slot.Add(-30 * time.Second)
	state.ForecastConsumedAt = slot.Add(-30 * time.Second)
	state.Forecasts = []requestforecast.Point{{At: slot, Until: slot.Add(30 * time.Second), Value: 120}, {At: slot.Add(30 * time.Second), Until: slot.Add(60 * time.Second), Value: 120}}
	state.Estimator = workloaddemand.NewEstimator(identity).Snapshot()
	state.Estimator.Last = workloaddemand.Estimate{State: "qualified", CPUSecondsPerRequest: .02, BackgroundCPUPerPod: .1, QualifiedWindows: 25, Version: 6, LastObservationAt: slot, FreshUntil: slot.Add(2 * time.Minute)}
	for i := 0; i < 60; i++ {
		at := slot.Add(time.Duration(i-60) * 30 * time.Second)
		value := float64(20 + i%7)
		state.Observations = append(state.Observations, requestforecast.Observation{At: at, Until: at.Add(30 * time.Second), ReceivedAt: at.Add(34 * time.Second), ArrivalsPerSecond: &value})
	}
	r.WorkloadRunner = demandIntegrationRunner(func(_, payload string, _ int) (string, error) {
		var input struct{ RequestedAt, FirstForecastAt time.Time }
		if err := json.Unmarshal([]byte(payload), &input); err != nil {
			t.Fatal(err)
		}
		result := publicationTrajectory(input.FirstForecastAt)
		result.Protocol, result.Unit, result.GeneratedAt = requestforecast.Protocol, requestforecast.Unit, input.RequestedAt
		result.TrainedThrough = slot
		*now = now.Add(6 * time.Second)
		body, err := json.Marshal(result)
		return string(body), err
	})
	if err = r.saveDemandState(ctx, cm, state); err != nil {
		t.Fatal(err)
	}
	request, target := int64(1000), int32(60)
	report := &api.ModelStatus{}
	n, deadline, usable := r.processRequestDemand(ctx, owner, model, scale, cpuMetricSnapshot{RequestPerPodMillicores: &request, TargetUtilizationPercentage: &target}, report, nil)
	if !usable || n != 5 || !deadline.Equal(slot.Add(60*time.Second)) || report.RequestDemand.Source != "request-demand" {
		t.Fatalf("late publication lost current coverage: n=%d usable=%v deadline=%v status=%+v", n, usable, deadline, report)
	}
	_, stored, err := r.loadDemandState(ctx, owner, model, identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.ForecastEvents) != 1 || len(stored.ForecastEvents[0].Forecasts) != 2 || len(stored.ForecastEvents[0].ComputedForecasts) != 3 || !stored.ForecastAvailableAt.Equal(*now) || !stored.ForecastConsumedAt.Equal(slot) {
		t.Fatalf("publication did not survive persisted-state restart: %+v", stored.ForecastEvents)
	}
	if !stored.Forecasts[0].At.Equal(slot.Add(30*time.Second)) || stored.Forecasts[0].Value != 120 {
		t.Fatalf("slow future fit replaced the current interval: %+v", stored.Forecasts)
	}
}
