package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/cgroup"
	metricadapter "github.com/cslab-ntua/HPA-Plus/internal/metrics"
	requestforecast "github.com/cslab-ntua/HPA-Plus/internal/prediction/requestdemand"
	"github.com/cslab-ntua/HPA-Plus/internal/workloaddemand"
	gathered "github.com/jthomperoo/k8shorizmetrics/v2/metrics"
	"github.com/jthomperoo/k8shorizmetrics/v2/metrics/podmetrics"
	resourceMetric "github.com/jthomperoo/k8shorizmetrics/v2/metrics/resource"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	scalefake "k8s.io/client-go/scale/fake"
	clienttesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestRequestDemandRequeueKeepsPublicationAndDecisionClocksIndependent(t *testing.T) {
	base := time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)
	request := &api.HPAPlus{Spec: api.HPAPlusSpec{Models: []api.Model{{RequestDemand: &api.RequestDemandConfig{}}}}}
	for _, tc := range []struct {
		name                        string
		at, regular, cooldown, want time.Duration
	}{
		{"before-publication", time.Second, 30 * time.Second, 0, 3 * time.Second},
		{"second-slot", 31 * time.Second, 30 * time.Second, 0, 3 * time.Second},
		{"already-published", 5 * time.Second, 30 * time.Second, 0, 29 * time.Second},
		{"exact-publication", 34 * time.Second, 30 * time.Second, 0, 30 * time.Second},
		{"earlier-decision", time.Second, time.Second, 0, time.Second},
		{"earlier-cooldown", 5 * time.Second, 30 * time.Second, 10 * time.Second, 5 * time.Second},
		{"expired-cooldown", 5 * time.Second, 30 * time.Second, time.Second, 29 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cooldown time.Time
			if tc.cooldown != 0 {
				cooldown = base.Add(tc.cooldown)
			}
			if got := requestDemandRequeue(request, base.Add(tc.at), tc.regular, cooldown); got != tc.want {
				t.Fatalf("requeue %v, want %v", got, tc.want)
			}
		})
	}
	legacy := &api.HPAPlus{Spec: api.HPAPlusSpec{Models: []api.Model{{Type: api.TypeXGBoost}}}}
	if got := requestDemandRequeue(legacy, base.Add(time.Second), 30*time.Second, base.Add(2*time.Second)); got != 30*time.Second {
		t.Fatal("request publication changed legacy model scheduling")
	}
}

type demandScheduleGatherer struct {
	now                 *time.Time
	reads, acquisitions int
}

func (*demandScheduleGatherer) CPUHistorySourceIdentity() string { return "schedule-test" }
func (g *demandScheduleGatherer) Gather(_ context.Context, specs []autoscalingv2.MetricSpec, _ string, _ labels.Selector, _, _ time.Duration) ([]*gathered.Metric, error) {
	g.reads++
	return []*gathered.Metric{{Spec: specs[0], Resource: &resourceMetric.Metric{
		PodMetricsInfo: podmetrics.MetricsInfo{"app-pod": {Timestamp: *g.now, Window: 30 * time.Second, Value: 600}},
		Requests:       map[string]int64{"app-pod": 1000}, ReadyPodCount: 1, TotalPods: 1, Timestamp: *g.now,
	}}}, nil
}
func (g *demandScheduleGatherer) GatherCgroupResources(context.Context, string, labels.Selector, time.Time, time.Time) (*cgroup.WorkloadObservation, error) {
	g.acquisitions++
	return nil, fmt.Errorf("deliberately missing observation, never fabricate CPU")
}
func (*demandScheduleGatherer) GatherCgroupPodResources(context.Context, string, string, string, time.Time, time.Time) (*cgroup.WorkloadObservation, error) {
	return nil, fmt.Errorf("no per-pod observation")
}

func TestRequestDemandCooldownRefreshesAndFitsButNeverWritesScale(t *testing.T) {
	r, owner, scale, now := demandIntegrationFixture(t)
	ctx := context.Background()
	period := 30000
	owner.Spec.SyncPeriod = &period
	mode := api.ModeControl
	owner.Spec.Mode = &mode
	owner.Status.LastScaleTime = &metav1.Time{Time: now.Add(-4 * time.Second)}
	if err := r.Client.Update(ctx, owner); err != nil {
		t.Fatal(err)
	}
	g := &demandScheduleGatherer{now: now}
	r.Gatherer, r.Evaluator = g, metricadapter.HPAEvaluator{}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{appsv1.SchemeGroupVersion})
	mapper.Add(appsv1.SchemeGroupVersion.WithKind("Deployment"), meta.RESTScopeNamespace)
	r.RESTMapper = mapper
	scale.ResourceVersion = "7"
	scales := &scalefake.FakeScaleClient{}
	scales.AddReactor("get", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) { return true, scale.DeepCopy(), nil })
	writes := 0
	scales.AddReactor("update", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
		writes++
		updated := action.(clienttesting.UpdateAction).GetObject().(*autoscalingv1.Scale)
		return true, updated.DeepCopy(), nil
	})
	r.ScaleClient = scales
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	r.TrafficPrometheusURL = server.URL
	model := &owner.Spec.Models[0]
	deployment := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "a", Name: "app"}, deployment); err != nil {
		t.Fatal(err)
	}
	raw, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(deployment)
	identity := demandHash([]interface{}{string(owner.UID), scaleTargetIdentity(owner, scale), model, raw["spec"].(map[string]interface{})["template"], demandMeasurementSource, server.URL})
	cm, state, err := r.loadDemandState(ctx, owner, model, identity)
	if err != nil {
		t.Fatal(err)
	}
	closed := now.UTC().Truncate(30 * time.Second)
	state.CreatedAt, state.LastSlot, state.LastFitSlot = now.Add(-time.Hour), closed.Add(-30*time.Second), closed.Add(-60*time.Second)
	state.Estimator = workloaddemand.NewEstimator(identity).Snapshot()
	state.Estimator.Last = workloaddemand.Estimate{State: "qualified", CPUSecondsPerRequest: .02, BackgroundCPUPerPod: .1,
		QualifiedWindows: 25, Version: 6, LastObservationAt: closed.Add(-30 * time.Second), FreshUntil: closed.Add(2 * time.Minute)}
	for i := 0; i < 60; i++ {
		at, value := closed.Add(time.Duration(i-61)*30*time.Second), float64(30+i%7)
		state.Observations = append(state.Observations, requestforecast.Observation{At: at, Until: at.Add(30 * time.Second), ReceivedAt: at.Add(34 * time.Second), ArrivalsPerSecond: &value})
	}
	if err := r.saveDemandState(ctx, cm, state); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ensureTargetHistory(ctx, owner, scale); err != nil {
		t.Fatal(err)
	}
	if err := r.mutateConfigMapData(ctx, owner, func(data *api.HPAPlusData) (bool, error) {
		data.ModelHistories[model.Name] = api.ModelHistory{Type: model.Type, HistoryEpoch: 1}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	fits := 0
	r.WorkloadRunner = demandIntegrationRunner(func(_, payload string, _ int) (string, error) {
		fits++
		var input struct {
			RequestedAt, FirstForecastAt time.Time
			Config                       requestforecast.Config
		}
		if err := json.Unmarshal([]byte(payload), &input); err != nil {
			return "", err
		}
		if !input.FirstForecastAt.Equal(closed.Add(30 * time.Second)) {
			t.Fatalf("publication skipped interval: %v", input.FirstForecastAt)
		}
		if len(input.Config.Features) == 0 || !sort.SliceIsSorted(input.Config.Features, func(i, j int) bool { return input.Config.Features[i].Name < input.Config.Features[j].Name }) {
			t.Fatal("worker feature schema is not deterministic by name")
		}
		result := requestforecast.Result{Protocol: requestforecast.Protocol, Unit: requestforecast.Unit, GeneratedAt: input.RequestedAt, Status: "ready", TrainedThrough: closed.Add(-30 * time.Second)}
		for i := 0; i < 3; i++ {
			at := input.FirstForecastAt.Add(time.Duration(i) * 30 * time.Second)
			result.Forecasts = append(result.Forecasts, requestforecast.Point{At: at, Until: at.Add(30 * time.Second), Value: 200})
		}
		body, err := json.Marshal(result)
		return string(body), err
	})
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(owner)})
	if err != nil {
		t.Fatal(err)
	}
	if g.acquisitions != 1 || fits != 1 {
		t.Fatalf("cooldown blocked acquisition/fit: acquired=%d fits=%d", g.acquisitions, fits)
	}
	if writes != 0 {
		t.Fatalf("cooldown wrote Scale %d times", writes)
	}
	if result.RequeueAfter != 26*time.Second {
		t.Fatalf("did not preserve cooldown expiry: %+v", result)
	}
	_, published, err := r.loadDemandState(ctx, owner, model, identity)
	if err != nil || !published.LastSlot.Equal(closed) || len(published.Forecasts) != 3 {
		t.Fatalf("refresh not persisted: %+v %v", published, err)
	}
	latest := &api.HPAPlus{}
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(owner), latest); err != nil {
		t.Fatal(err)
	}
	if !latest.Status.LastScaleTime.Equal(owner.Status.LastScaleTime) || latest.Status.LastDecision != nil {
		t.Fatal("cooldown changed scale accounting")
	}
	*now = closed.Add(31 * time.Second)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(owner)}); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("write not permitted after cooldown: %d", writes)
	}
	if fits != 1 {
		t.Fatalf("same closed interval fitted again: %d", fits)
	}
}

func TestLegacyCooldownStillSkipsMetricAndModelEvaluation(t *testing.T) {
	r, owner, scale, now := demandIntegrationFixture(t)
	owner.Spec.Models[0].RequestDemand = nil
	owner.Status.LastScaleTime = &metav1.Time{Time: now.Add(-4 * time.Second)}
	if err := r.Client.Update(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	g := &demandScheduleGatherer{now: now}
	r.Gatherer = g
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{appsv1.SchemeGroupVersion})
	mapper.Add(appsv1.SchemeGroupVersion.WithKind("Deployment"), meta.RESTScopeNamespace)
	r.RESTMapper = mapper
	scales := &scalefake.FakeScaleClient{}
	scales.AddReactor("get", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) { return true, scale.DeepCopy(), nil })
	r.ScaleClient = scales
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(owner)})
	if err != nil {
		t.Fatal(err)
	}
	if g.reads != 0 || g.acquisitions != 0 || result.RequeueAfter != syncPeriodForInstance(owner)-4*time.Second {
		t.Fatalf("legacy cooldown changed: reads=%d acquired=%d result=%+v", g.reads, g.acquisitions, result)
	}
}

func TestRequestSourceReflectsFinalCandidateAfterExpiryOrHistoryFence(t *testing.T) {
	for _, source := range []string{"request-demand", "cpu-forecast"} {
		statuses := []api.ModelStatus{{Name: "xgb", State: "Expired", RequestDemand: &api.RequestDemandStatus{Source: source}}}
		finalizeRequestDemandSources(statuses, nil)
		if statuses[0].RequestDemand.Source != "hold" || statuses[0].RequestDemand.FallbackReason == "" {
			t.Fatalf("discarded forecast remained active: %+v", statuses[0])
		}
	}
	statuses := []api.ModelStatus{{Name: "request", RequestDemand: &api.RequestDemandStatus{Source: "request-demand"}}, {Name: "cpu", RequestDemand: &api.RequestDemandStatus{Source: "hold"}}}
	finalizeRequestDemandSources(statuses, map[string]int32{"request": 4, "cpu": 3})
	if statuses[0].RequestDemand.Source != "request-demand" || statuses[1].RequestDemand.Source != "cpu-forecast" {
		t.Fatal("surviving candidate source changed")
	}
}

func TestMissingDemandResourceWindowDoesNotForwardFillFeatures(t *testing.T) {
	until := time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)
	zero := 0.
	memory := uint64(100)
	for _, tc := range []struct {
		name string
		cpu  *cgroup.WorkloadObservation
		err  error
	}{
		{"failed", nil, fmt.Errorf("collector unavailable")},
		{"absent", nil, nil},
		{"incomplete", &cgroup.WorkloadObservation{Complete: false, CPUSeconds: 20, MemoryBytes: &memory, ThrottledSeconds: &zero, PressureSomeSeconds: &zero}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			context := map[string]requestforecast.FeatureValue{}
			row := requestforecast.Observation{Until: until, Features: map[string]requestforecast.FeatureValue{}}
			old := feature(5, "old-unit", until.Add(-30*time.Second), until.Add(-26*time.Second))
			for _, name := range []string{"cpu_usage", "memory_usage", "cpu_throttled", "cpu_pressure"} {
				context[name] = old
				row.Features[name] = old
			}
			recordDemandResourceFeatures(context, &row, tc.cpu, tc.err, until.Add(4*time.Second))
			for name, value := range context {
				if value.Value != nil || row.Features[name].Value != nil || !value.SourceAt.Equal(until) || !value.ReceivedAt.Equal(until.Add(4*time.Second)) {
					t.Fatalf("stale %s feature forwarded: context=%+v row=%+v", name, value, row.Features[name])
				}
			}
			if old.Value == nil || *old.Value != 5 {
				t.Fatal("historical real observation was modified")
			}
		})
	}
	context := map[string]requestforecast.FeatureValue{"memory_usage": feature(100, "bytes", until.Add(-30*time.Second), until.Add(-26*time.Second))}
	row := requestforecast.Observation{Until: until, Features: map[string]requestforecast.FeatureValue{}}
	recordDemandResourceFeatures(context, &row, &cgroup.WorkloadObservation{Complete: true, From: until.Add(-30 * time.Second), To: until, SourceAt: until, CPUSeconds: 0, ThrottledSeconds: &zero}, nil, until.Add(4*time.Second))
	if row.Features["cpu_usage"].Value == nil || *row.Features["cpu_usage"].Value != 0 || row.Features["cpu_throttled"].Value == nil {
		t.Fatal("genuine zero became missing")
	}
	if context["memory_usage"].Value != nil || row.Features["memory_usage"].Value != nil {
		t.Fatal("absent optional reading kept old value")
	}
	later := until.Add(time.Second)
	recordDemandResourceFeatures(context, &row, &cgroup.WorkloadObservation{Complete: true, From: until.Add(-30 * time.Second), To: until, SourceAt: later, ReceivedAt: until.Add(3 * time.Second), CPUSeconds: 30}, nil, until.Add(4*time.Second))
	if context["cpu_usage"].Value == nil || row.Features["cpu_usage"].Value == nil || row.Features["cpu_usage"].MeasurementWindow == nil || !row.Features["cpu_usage"].SourceAt.Equal(later) || !row.Features["cpu_usage"].ReceivedAt.Equal(until.Add(3*time.Second)) {
		t.Fatal("honest late backing endpoint was discarded or backdated")
	}
}

func TestScaleUpDoesNotWaitForWriteCooldown(t *testing.T) {
	now := time.Date(2026, 10, 21, 19, 17, 4, 0, time.UTC)
	cooldown := now.Add(28 * time.Second)
	// r15 surprise: 2 -> 4 at 19:17:02 left the floor's 9 pods waiting 30 s.
	if deferredByCooldown(now, cooldown, 9, 4, false) || deferredByCooldown(now, cooldown, 9, 4, true) {
		t.Fatal("a scale-up waited for the write cooldown")
	}
	if !deferredByCooldown(now, cooldown, 4, 4, false) || !deferredByCooldown(now, cooldown, 4, 4, true) || !deferredByCooldown(now, cooldown, 3, 4, false) {
		t.Fatal("holding capacity, or removing pods just added, must still wait for the cooldown")
	}
	if deferredByCooldown(cooldown, cooldown, 3, 4, false) {
		t.Fatal("an expired cooldown deferred a decision")
	}
}

func TestScaleDownAfterScaleDownDoesNotWaitForWriteCooldown(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 49, 15, 0, time.UTC)
	at := func(seconds int) *metav1.Time {
		return &metav1.Time{Time: now.Add(time.Duration(seconds) * time.Second)}
	}
	// r24 peak at 12:48: 5 -> 4 while it ended, then 4 pods held 15 s after it although 2 were wanted.
	status := api.HPAPlusStatus{
		ScaleUpEventHistory:   []api.TimestampedSample{{Time: at(-75), Replicas: 4}},
		ScaleDownEventHistory: []api.TimestampedSample{{Time: at(-15), Replicas: 1}},
	}
	if !lastWriteRemovedPods(status) || deferredByCooldown(now, now.Add(15*time.Second), 2, 4, lastWriteRemovedPods(status)) {
		t.Fatal("a scale-down after a scale-down waited for the write cooldown")
	}
	status.ScaleUpEventHistory = append(status.ScaleUpEventHistory, api.TimestampedSample{Time: at(-10), Replicas: 1})
	if lastWriteRemovedPods(status) || !deferredByCooldown(now, now.Add(20*time.Second), 3, 4, lastWriteRemovedPods(status)) {
		t.Fatal("pods added by the newest write were removed inside the cooldown")
	}
	if lastWriteRemovedPods(api.HPAPlusStatus{}) {
		t.Fatal("no recorded write counted as a scale-down")
	}
}

func TestReconstructedDemandDecidesAtLeastOncePerFloorWindow(t *testing.T) {
	base := time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)
	period := 30000
	reconstructed := &api.HPAPlus{Spec: api.HPAPlusSpec{SyncPeriod: &period, Models: []api.Model{{RequestDemand: &api.RequestDemandConfig{ForecastTarget: api.RequestDemandTargetReconstructedCPU}}}}}
	if got := requestDemandRequeue(reconstructed, base.Add(5*time.Second), 30*time.Second, time.Time{}); got != 5*time.Second {
		t.Fatalf("reconstructed floor rechecked after %v, want 5s", got)
	}
	if got := requestDemandRequeue(reconstructed, base.Add(time.Second), 30*time.Second, time.Time{}); got != 3*time.Second {
		t.Fatalf("publication no longer comes first: %v", got)
	}
}
