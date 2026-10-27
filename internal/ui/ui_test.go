package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/history"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var testNow = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func int32p(v int32) *int32 { return &v }
func int64p(v int64) *int64 { return &v }

func testHPAPlus(name string) *api.HPAPlus {
	return &api.HPAPlus{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name, UID: types.UID("uid-" + name)},
		Spec: api.HPAPlusSpec{MinReplicas: int32p(1), MaxReplicas: 8,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: name},
			Models:         []api.Model{{Name: "xgboost", Type: "XGBoost"}}},
		Status: api.HPAPlusStatus{CurrentReplicas: 2, LastDecision: &api.DecisionStatus{Mode: "control", CandidateMode: "predictive",
			ScaleWrite: "accepted", EvaluatedAt: metav1.NewTime(testNow.Add(-5 * time.Second)), RequestedReplicas: 2, DesiredReplicas: 3}},
	}
}

func ownedBy(owner *api.HPAPlus) []metav1.OwnerReference {
	return []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "HPAPlus", Name: owner.Name, UID: owner.UID, Controller: func() *bool { b := true; return &b }()}}
}

// historyConfigMap stores three 30 s CPU windows and a two-window forecast.
// extra is spliced into the first sample, to imitate another version's fields.
func historyConfigMap(t *testing.T, owner *api.HPAPlus, extra string) *corev1.ConfigMap {
	t.Helper()
	samples := []api.TimestampedSample{}
	for i, cores := range []int64{400, 900, 1300} {
		at := metav1.NewTime(testNow.Add(time.Duration(i-3) * 30 * time.Second))
		samples = append(samples, api.TimestampedSample{Time: &at, Replicas: int32(i + 1), TotalCPUUsageMillicores: int64p(cores),
			RequestPerPodMillicores: int64p(1000), TargetCPUUtilizationPercentage: int32p(60)})
	}
	start := metav1.NewTime(testNow.Add(-time.Hour))
	data := &api.HPAPlusData{ModelHistories: map[string]api.ModelHistory{"xgboost": {Type: "XGBoost", ReplicaHistory: samples, StartTime: &start,
		CachedPrediction: &api.CachedPrediction{Replicas: 3, Trajectory: []api.CPUForecastPoint{
			{At: testNow.Format(time.RFC3339Nano), Until: testNow.Add(30 * time.Second).Format(time.RFC3339Nano), CPU: 1500},
			{At: testNow.Add(30 * time.Second).Format(time.RFC3339Nano), Until: testNow.Add(time.Minute).Format(time.RFC3339Nano), CPU: 1700},
		}}}}}
	raw, err := history.Encode(data)
	if err != nil {
		t.Fatal(err)
	}
	payload := string(raw)
	if extra != "" {
		payload = strings.Replace(payload, `"replicas":1`, extra+`,"replicas":1`, 1)
	}
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: owner.Namespace, Name: "hpa-plus-" + owner.Name + "-data", OwnerReferences: ownedBy(owner)},
		Data: map[string]string{"data": payload}}
}

func deployment(name string, replicas, ready int32) *appsv1.Deployment {
	limits := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1500m")}
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name}, Spec: appsv1.DeploymentSpec{Replicas: &replicas,
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{Limits: limits}},
			{Name: "sidecar", Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")}}}}}}},
		Status: appsv1.DeploymentStatus{ReadyReplicas: ready}}
}

func testReader(objects ...client.Object) *Reader {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(api.AddToScheme(scheme))
	return &Reader{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), Now: func() time.Time { return testNow }}
}

func TestHealthReadsTheLatestReconciliation(t *testing.T) {
	condition := func(kind, status, reason string, since time.Duration) metav1.Condition {
		return metav1.Condition{Type: kind, Status: metav1.ConditionStatus(status), Reason: reason, Message: kind + " " + reason,
			LastTransitionTime: metav1.NewTime(testNow.Add(-since))}
	}
	cases := map[string]struct {
		change func(*api.HPAPlus)
		want   string
	}{
		"working":         {func(*api.HPAPlus) {}, "healthy"},
		"no decision yet": {func(h *api.HPAPlus) { h.Status.LastDecision = nil }, "waiting"},
		"old decision": {func(h *api.HPAPlus) {
			h.Status.LastDecision.EvaluatedAt = metav1.NewTime(testNow.Add(-10 * time.Minute))
		}, "idle"},
		"old decision, old problems": {func(h *api.HPAPlus) {
			h.Status.LastDecision.EvaluatedAt = metav1.NewTime(testNow.Add(-time.Hour))
			h.Status.Conditions = []metav1.Condition{condition("SamplingActive", "False", "SamplingFailed", time.Hour)}
		}, "idle"},
		"new pods without metrics": {func(h *api.HPAPlus) {
			h.Status.Conditions = []metav1.Condition{condition("MetricsAvailable", "False", "PartialMetrics", time.Minute)}
		}, "partial"},
		"metrics missing for long": {func(h *api.HPAPlus) {
			h.Status.Conditions = []metav1.Condition{condition("MetricsAvailable", "False", "PartialMetrics", 10*time.Minute)}
		}, "attention"},
		"observe mode": {func(h *api.HPAPlus) {
			h.Status.Conditions = []metav1.Condition{condition("ScalingActive", "False", "ObserveOnly", time.Hour)}
		}, "healthy"},
		"scaling failed": {func(h *api.HPAPlus) {
			h.Status.Conditions = []metav1.Condition{condition("ScalingActive", "False", "ScaleWriteFailed", time.Minute)}
		}, "attention"},
		"write rejected": {func(h *api.HPAPlus) { h.Status.LastDecision.ScaleWrite = "rejected" }, "attention"},
		"model learning": {func(h *api.HPAPlus) {
			h.Status.ModelStatuses = []api.ModelStatus{{Name: "xgboost", State: "WarmingUp", HistorySamples: 12, RequiredSamples: 60}}
		}, "learning"},
		"model failing": {func(h *api.HPAPlus) { h.Status.ModelStatuses = []api.ModelStatus{{Name: "xgboost", State: "Backoff"}} }, "attention"},
	}
	for name, tc := range cases {
		item := testHPAPlus("web")
		tc.change(item)
		got := (&Reader{Now: func() time.Time { return testNow }}).summary(item)
		if got.Health != tc.want {
			t.Errorf("%s: health %q (%s), want %q", name, got.Health, got.Reason, tc.want)
		}
	}
}

func TestDetailShowsTargetHistoryAndForecast(t *testing.T) {
	owner := testHPAPlus("web")
	r := testReader(owner, historyConfigMap(t, owner, ""), deployment("web", 3, 2))
	d, err := r.Detail(context.Background(), "shop", "web")
	if err != nil {
		t.Fatal(err)
	}
	if d.Replicas != 3 || d.Ready == nil || *d.Ready != 2 || d.Desired == nil || *d.Desired != 3 {
		t.Fatalf("target and decision: replicas %d ready %v desired %v", d.Replicas, d.Ready, d.Desired)
	}
	if d.LimitCoresPerPod == nil || *d.LimitCoresPerPod != 1.75 {
		t.Fatalf("pod CPU limit %v, want 1.75 (both containers)", d.LimitCoresPerPod)
	}
	unlimited := deployment("web", 3, 2)
	unlimited.Spec.Template.Spec.Containers[1].Resources = corev1.ResourceRequirements{}
	if limit := podCPULimit(mustUnstructured(t, unlimited)); limit != nil {
		t.Fatalf("a container without a limit leaves the pod unlimited, got %v", *limit)
	}
	if d.HistoryError != "" || len(d.Series) != 1 || len(d.Series[0].History) != 3 {
		t.Fatalf("history: %q %+v", d.HistoryError, d.Series)
	}
	last := d.Series[0].History[2]
	if last.Cores != 1.3 || last.CapacityCores == nil || *last.CapacityCores != 3*1*0.6 || d.CPUCores == nil || *d.CPUCores != 1.3 {
		t.Fatalf("latest sample %+v cpu %v", last, d.CPUCores)
	}
	if d.Capacity == nil || d.Capacity.RequestCores != 1 || d.Capacity.TargetPercent != 60 {
		t.Fatalf("capacity %+v", d.Capacity)
	}
	if f := d.Series[0].Forecast; len(f) != 2 || f[1].Value != 1.7 || !f[1].Until.Equal(testNow.Add(time.Minute)) {
		t.Fatalf("forecast %+v", f)
	}
}

func TestHistoryFromAnotherVersionIsStillShown(t *testing.T) {
	owner := testHPAPlus("web")
	cm := historyConfigMap(t, owner, `"cpuObservation":{"source":"newer"}`)
	if _, err := history.Decode(cm.Data["data"]); err == nil {
		t.Fatal("the operator's decoder should reject the unknown field this test relies on")
	}
	d, err := testReader(owner, cm, deployment("web", 3, 3)).Detail(context.Background(), "shop", "web")
	if err != nil || d.HistoryError != "" || len(d.Series) != 1 || len(d.Series[0].History) != 3 {
		t.Fatalf("detail %v %q %+v", err, d.HistoryError, d.Series)
	}
}

func TestHistoryOwnedBySomethingElseIsNotShown(t *testing.T) {
	owner := testHPAPlus("web")
	cm := historyConfigMap(t, owner, "")
	cm.OwnerReferences[0].UID = "someone-else"
	d, err := testReader(owner, cm, deployment("web", 3, 3)).Detail(context.Background(), "shop", "web")
	if err != nil || d.HistoryError == "" || len(d.Series) != 0 {
		t.Fatalf("detail %v %q %+v", err, d.HistoryError, d.Series)
	}
}

func TestRequestWindowsShowDemandBeyondMeasuredCPU(t *testing.T) {
	owner := testHPAPlus("shop-api")
	owner.Spec.Models[0].RequestDemand = &api.RequestDemandConfig{ForecastTarget: "reconstructedCPU"}
	state := map[string]interface{}{
		"forecastIssuedAt": testNow,
		"observations": []map[string]interface{}{{
			"at": testNow.Add(-30 * time.Second), "until": testNow, "arrivalsPerSecond": 30.0,
			"features": map[string]interface{}{
				"traffic_arrivals": map[string]interface{}{"value": 30.0}, "traffic_completions": map[string]interface{}{"value": 20.0},
				"cpu_usage": map[string]interface{}{"value": 1.2}, "traffic_latency_p95": map[string]interface{}{"value": 0.25},
				"traffic_errors": map[string]interface{}{"value": nil}, "ready_replicas": map[string]interface{}{"value": 2.0},
			},
		}},
		"forecasts": []map[string]interface{}{{"at": testNow, "until": testNow.Add(30 * time.Second), "value": 1.9}},
	}
	raw, _ := json.Marshal(state)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: demandStateName(owner, "xgboost"), OwnerReferences: ownedBy(owner)},
		Data: map[string]string{"state": string(raw)}}
	d, err := testReader(owner, cm, deployment("shop-api", 2, 2)).Detail(context.Background(), "shop", "shop-api")
	if err != nil || len(d.Demand) != 1 {
		t.Fatalf("demand %v %+v", err, d.Demand)
	}
	series := d.Demand[0]
	if series.Target != "reconstructedCPU" || series.IssuedAt == nil || len(series.Forecast) != 1 || series.Forecast[0].Value != 1.9 {
		t.Fatalf("series %+v", series)
	}
	w := series.Windows[0]
	if w.Needed == nil || *w.Needed != 1.2*30/20 || w.Errors != nil || *w.LatencyP95 != 0.25 || *w.Ready != 2 {
		t.Fatalf("window %+v", w)
	}
}

func TestServerIsReadOnlyAndBoundToItsHostNames(t *testing.T) {
	owner := testHPAPlus("web")
	s := &Server{Reader: testReader(owner, historyConfigMap(t, owner, ""), deployment("web", 3, 3)), Namespace: "shop", Hosts: []string{"hpaplus.localtest.me", "127.0.0.1"}}
	handler := s.Handler()
	call := func(method, host, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Host = host
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := call("GET", "hpaplus.localtest.me:8080", "/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "app.js") ||
		!strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("index: %d %q", rec.Code, rec.Header())
	}
	for _, asset := range []string{"/app.js", "/charts.js", "/app.css"} {
		if rec := call("GET", "127.0.0.1:8080", asset); rec.Code != http.StatusOK {
			t.Errorf("%s: %d", asset, rec.Code)
		}
	}
	if rec := call("GET", "attacker.example:8080", "/api/hpapluses"); rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("another host name: %d", rec.Code)
	}
	if rec := call("POST", "hpaplus.localtest.me:8080", "/api/hpapluses"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
	rec := call("GET", "hpaplus.localtest.me:8080", "/api/hpapluses?namespace=elsewhere")
	var list struct {
		Namespace string    `json:"namespace"`
		Items     []Summary `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || list.Namespace != "shop" || len(list.Items) != 1 || list.Items[0].Name != "web" {
		t.Fatalf("list in the fixed namespace: %d %s", rec.Code, rec.Body)
	}
	if rec := call("GET", "hpaplus.localtest.me:8080", "/api/hpapluses/elsewhere/web"); rec.Code != http.StatusNotFound {
		t.Errorf("detail outside the fixed namespace: %d", rec.Code)
	}
	if rec := call("GET", "hpaplus.localtest.me:8080", "/api/hpapluses/shop/missing"); rec.Code != http.StatusNotFound {
		t.Errorf("missing HPAPlus: %d", rec.Code)
	}
	rec = call("GET", "hpaplus.localtest.me:8080", "/api/hpapluses/shop/web/trend")
	var trend struct {
		Samples []Sample `json:"samples"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &trend); err != nil || len(trend.Samples) != 3 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("trend: %d %s", rec.Code, rec.Body)
	}
}

func TestMarkersShowWhenForecastsWereReadyAndTheModeChanged(t *testing.T) {
	created := metav1.NewTime(testNow.Add(-time.Hour))
	ready := metav1.NewTime(testNow.Add(-20 * time.Minute))
	switched := metav1.NewTime(testNow.Add(-10 * time.Minute))
	later := metav1.NewTime(testNow.Add(-5 * time.Minute))
	entry := func(at metav1.Time, fields string, subresource string) metav1.ManagedFieldsEntry {
		return metav1.ManagedFieldsEntry{Manager: "m", Operation: metav1.ManagedFieldsOperationUpdate, Time: &at, Subresource: subresource,
			FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(fields)}}
	}
	createdEntry := entry(created, `{"f:spec":{".":{},"f:mode":{},"f:maxReplicas":{}}}`, "")
	item := testHPAPlus("web")
	item.CreationTimestamp = created
	item.Status.Conditions = []metav1.Condition{{Type: "PredictionsAvailable", Status: metav1.ConditionTrue, LastTransitionTime: ready}}
	control := api.ModeControl
	item.Spec.Mode = &control

	item.ManagedFields = []metav1.ManagedFieldsEntry{createdEntry}
	if got := markers(item); len(got) != 1 || got[0].Label != "Forecasts ready" || !got[0].At.Equal(ready.Time) {
		t.Fatalf("created in its mode: %+v", got)
	}
	item.ManagedFields = []metav1.ManagedFieldsEntry{createdEntry, entry(switched, `{"f:spec":{"f:mode":{}}}`, ""),
		entry(later, `{"f:status":{"f:currentReplicas":{}}}`, "status")}
	if got := markers(item); len(got) != 2 || got[1].Label != "Took control" || !got[1].At.Equal(switched.Time) {
		t.Fatalf("mode switched alone: %+v", got)
	}
	item.ManagedFields = []metav1.ManagedFieldsEntry{entry(later, `{"f:spec":{".":{},"f:mode":{},"f:maxReplicas":{}}}`, "")}
	if got := markers(item); len(got) != 2 || got[1].Label != "Settings changed" || !got[1].At.Equal(later.Time) {
		t.Fatalf("mode applied with other settings: %+v", got)
	}
}

func mustUnstructured(t *testing.T, obj runtime.Object) map[string]interface{} {
	t.Helper()
	out, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
