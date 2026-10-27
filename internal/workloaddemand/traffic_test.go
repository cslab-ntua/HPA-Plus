package workloaddemand

import (
	"context"
	"encoding/json"
	"fmt"
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testConfig() api.RequestDemandConfig {
	return api.RequestDemandConfig{Traffic: api.TrafficProviderConfig{Provider: "prometheus", ArrivalsMetric: "app_started_total", CompletionsMetric: "app_finished_total", ArrivalSemantics: "started", CompletionSemantics: "finished", RetrySemantics: "attempts"}}
}
func TestNativeCounterIntervalsAndWorkloadScope(t *testing.T) {
	from := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	to := from.Add(30 * time.Second)
	client := mockHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		for _, part := range []string{`namespace="tenant-a"`, `pod=~"(app-1)"`, `pod_uid=~"(uid-1)"`} {
			if !strings.Contains(q, part) {
				t.Errorf("unscoped query: %s", q)
			}
		}
		values := [][]any{}
		for i := -1; i <= 15; i++ {
			values = append(values, []any{float64(from.Add(time.Duration(i*2) * time.Second).Unix()), fmt.Sprint(1000 + i*20)})
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{map[string]any{"metric": map[string]string{"namespace": "tenant-a", "pod": "app-1", "pod_uid": "uid-1"}, "values": values}}}})
	}))

	reader := TrafficReader{URL: "http://prometheus.invalid", Client: client, Now: func() time.Time { return to.Add(time.Second) }}
	got, err := reader.ReadWindow(context.Background(), Scope{Namespace: "tenant-a", Pods: []PodIdentity{{Name: "app-1", UID: "uid-1"}}}, testConfig(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Qualified || got.ArrivalsPerSecond != 10 || got.CompletedRequests != 300 {
		t.Fatalf("native counter accounting: %+v", got)
	}
	if !got.PerPodIntervals[0].Start.Equal(from) || !got.PerPodIntervals[0].End.Equal(to) {
		t.Fatalf("counter native window was relabelled: %+v", got.PerPodIntervals)
	}
}
func TestCounterResetAndNewContainerRemainMissing(t *testing.T) {
	start := time.Unix(1000, 0)
	for _, samples := range [][]promSample{{{At: start.Add(time.Second), Value: 1}, {At: start.Add(2 * time.Second), Value: 2}}, {{At: start, Value: 100}, {At: start.Add(2 * time.Second), Value: 0}, {At: start.Add(30 * time.Second), Value: 200}}} {
		if _, ok := counterDelta(samples, start, start.Add(30*time.Second), 3*time.Second); ok {
			t.Fatal("new/reset counter was fabricated into a measurement")
		}
	}
}
func TestUIDIsolationAndTelemetrySemantics(t *testing.T) {
	config := testConfig()
	config.Traffic.ArrivalSemantics = "finished"
	if ValidateConfig(config) == nil {
		t.Fatal("completions accepted as arrivals")
	}
	config = testConfig()
	config.Traffic.AdditionalLabels = map[string]string{"namespace": "other"}
	if ValidateConfig(config) == nil {
		t.Fatal("scope override accepted")
	}
	config = testConfig()
	config.Traffic.ArrivalsMetric = `rate(secret[1m])`
	if ValidateConfig(config) == nil {
		t.Fatal("arbitrary query accepted")
	}
	now := time.Now().UTC()
	client := mockHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"namespace":"tenant-a","pod":"app-1","pod_uid":"old-uid"},"values":[]}]}}`)
	}))

	reader := TrafficReader{URL: "http://prometheus.invalid", Client: client}
	_, err := reader.ReadWindow(context.Background(), Scope{Namespace: "tenant-a", Pods: []PodIdentity{{Name: "app-1", UID: "new-uid"}}}, testConfig(), now.Add(-time.Minute), now.Add(-30*time.Second))
	if err == nil {
		t.Fatal("recreated pod accepted old UID series")
	}
}

func TestSameNameReplacementUIDsHaveIndependentCountersAndCoverage(t *testing.T) {
	from := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	to := from.Add(30 * time.Second)
	scope := Scope{Namespace: "tenant-a", Pods: []PodIdentity{{Name: "app-0", UID: "old-uid"}, {Name: "app-0", UID: "new-uid"}}}
	config := testConfig()
	config.Traffic.ErrorsMetric = "app_errors_total"
	config.Traffic.LatencyBucketMetric = "app_latency_bucket"
	config.ExtraMetrics = []api.DemandExtraMetric{{Name: "queue", Metric: "app_queue", Unit: "requests", Kind: "gauge"}}
	for _, missingOld := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing_old_%t", missingOld), func(t *testing.T) {
			client := mockHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query().Get("query")
				metric := strings.SplitN(q, "{", 2)[0]
				result := []any{}
				for index, uid := range []string{"old-uid", "new-uid"} {
					if missingOld && index == 0 {
						continue
					}
					bounds := []string{""}
					if metric == "app_latency_bucket" {
						bounds = []string{"0.5", "+Inf"}
					}
					for _, bound := range bounds {
						labels := map[string]string{"namespace": "tenant-a", "pod": "app-0", "pod_uid": uid}
						if bound != "" {
							labels["le"] = bound
						}
						values := [][]any{}
						for i := 0; i <= 15; i++ {
							value := float64(1000 + i*20*(index+1))
							if metric == "app_errors_total" {
								value = 0
							} else if metric == "app_queue" {
								value = float64(3 + index*4)
							} else if bound == "0.5" {
								value /= 2
							}
							values = append(values, []any{from.Add(time.Duration(i*2) * time.Second).Unix(), fmt.Sprint(value)})
						}
						result = append(result, map[string]any{"metric": labels, "values": values})
					}
				}
				json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": result}})
			}))
			reader := TrafficReader{URL: "http://prometheus.invalid", Client: client, Now: func() time.Time { return to.Add(time.Second) }}
			got, err := reader.ReadWindow(context.Background(), scope, config, from, to)
			if err != nil {
				t.Fatal(err)
			}
			if missingOld {
				if got.Qualified || got.ArrivalCoverage != 0 || got.Coverage != .5 || len(got.PerPodIntervals) != 1 || got.PerPodIntervals[0].UID != "new-uid" {
					t.Fatalf("missing old UID was replaced by new UID or counted twice: %+v", got)
				}
				for _, key := range []string{"traffic_arrivals", "traffic_completions", "traffic_errors", "traffic_latency_p95", "queue"} {
					if got.Features[key].Value != nil {
						t.Fatalf("%s claimed complete replacement coverage: %+v", key, got.Features[key])
					}
				}
				return
			}
			if !got.Qualified || got.ArrivalsPerSecond != 30 || got.CompletedRequests != 900 || len(got.PerPodIntervals) != 2 {
				t.Fatalf("same-name UID accounting: %+v", got)
			}
			if got.PerPodIntervals[0].UID != "old-uid" || got.PerPodIntervals[1].UID != "new-uid" || got.PerPodIntervals[0].CompletedRequests != 300 || got.PerPodIntervals[1].CompletedRequests != 600 {
				t.Fatalf("replacement counters were merged: %+v", got.PerPodIntervals)
			}
			if value := got.Features["queue"].Value; value == nil || *value != 10 {
				t.Fatalf("replacement gauge population lost: %+v", got.Features["queue"])
			}
			if got.Features["traffic_latency_p95"].Value == nil || got.Features["traffic_errors"].Value == nil {
				t.Fatalf("same-name identities lost in optional feature coverage: %+v", got.Features)
			}
		})
	}
}

func TestResolvedPodIdentityPairsCannotCrossWorkloads(t *testing.T) {
	from := time.Unix(1000, 0)
	to := from.Add(30 * time.Second)
	scope := Scope{Namespace: "tenant-a", Pods: []PodIdentity{{Name: "app-1", UID: "uid-1"}, {Name: "app-2", UID: "uid-2"}}}
	for _, labels := range []map[string]string{
		{"namespace": "tenant-b", "pod": "app-1", "pod_uid": "uid-1"},
		{"namespace": "tenant-a", "pod": "app-1", "pod_uid": "unrelated-uid"},
		{"namespace": "tenant-a", "pod": "app-1", "pod_uid": "uid-2"},
	} {
		client := mockHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{map[string]any{"metric": labels, "values": []any{}}}}})
		}))
		reader := TrafficReader{URL: "http://prometheus.invalid", Client: client, Now: func() time.Time { return to.Add(time.Second) }}
		if _, err := reader.ReadWindow(context.Background(), scope, testConfig(), from, to); err == nil {
			t.Fatalf("accepted series outside exact namespace/name/UID pairs: %+v", labels)
		}
	}
}

func TestFinishedOutcomeErrorFractionKeepsProcessedWorkSeparate(t *testing.T) {
	from := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	to := from.Add(30 * time.Second)
	for _, tc := range []struct {
		name                             string
		omitOutcomes, missingOutcomes    bool
		wrongUID, resetOutcomes          bool
		outcomeFrom, outcomeTo, errorTo  int
		outcomeRate, workRate, errorRate float64
		want                             *float64
	}{
		{name: "all finished includes rejected work", outcomeTo: 30, errorTo: 30, outcomeRate: 100, workRate: 90, errorRate: 10, want: floatPointer(.1)},
		{name: "zero errors is genuine zero", outcomeTo: 30, errorTo: 30, outcomeRate: 100, workRate: 100, want: floatPointer(0)},
		{name: "no processed work does not hide outcomes", outcomeTo: 30, errorTo: 30, outcomeRate: 100, errorRate: 100, want: floatPointer(1)},
		{name: "optional denominator absent", omitOutcomes: true, outcomeTo: 30, errorTo: 30, outcomeRate: 100, workRate: 90, errorRate: 10},
		{name: "missing denominator", missingOutcomes: true, outcomeTo: 30, errorTo: 30, workRate: 90, errorRate: 10},
		{name: "recreated UID denominator", wrongUID: true, outcomeTo: 30, errorTo: 30, outcomeRate: 100, workRate: 90, errorRate: 10},
		{name: "stale denominator", outcomeTo: 24, errorTo: 30, outcomeRate: 100, workRate: 90, errorRate: 10},
		{name: "zero denominator", outcomeTo: 30, errorTo: 30, workRate: 90, errorRate: 10},
		{name: "errors exceed outcomes", outcomeTo: 30, errorTo: 30, outcomeRate: 9, workRate: 90, errorRate: 10},
		{name: "reset denominator", resetOutcomes: true, outcomeTo: 30, errorTo: 30, outcomeRate: 100, workRate: 90, errorRate: 10},
		// Each counter alone has 29/30 coverage, but their common support is 28/30.
		{name: "different missing support cannot make a ratio", outcomeFrom: 1, outcomeTo: 30, errorTo: 29, outcomeRate: 100, workRate: 90, errorRate: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := testConfig()
			config.Traffic.ErrorsMetric = "app_errors_total"
			if !tc.omitOutcomes {
				config.Traffic.OutcomesMetric = "app_outcomes_total"
			}
			queriedOutcomes := false
			client := mockHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				query := r.URL.Query().Get("query")
				for _, scope := range []string{`namespace="tenant-a"`, `pod=~"(app-1)"`, `pod_uid=~"(uid-1)"`} {
					if !strings.Contains(query, scope) {
						t.Errorf("unscoped optional counter: %s", query)
					}
				}
				metric := strings.SplitN(query, "{", 2)[0]
				result := []any{}
				start, end, rate := 0, 30, 120.
				uid := "uid-1"
				if metric == config.Traffic.CompletionsMetric {
					rate = tc.workRate
				} else if metric == config.Traffic.ErrorsMetric {
					rate, end = tc.errorRate, tc.errorTo
				} else if metric == "app_outcomes_total" {
					queriedOutcomes = true
					start, end, rate = tc.outcomeFrom, tc.outcomeTo, tc.outcomeRate
					if tc.wrongUID {
						uid = "old-uid"
					}
				}
				if metric != "app_outcomes_total" || !tc.missingOutcomes {
					values := [][]any{}
					for i := start; i <= end; i++ {
						if metric == "app_outcomes_total" && tc.resetOutcomes && i == 15 {
							continue
						}
						value := 1000 + float64(i)*rate
						if metric == "app_outcomes_total" && tc.resetOutcomes && i >= 15 {
							value -= 1000 + 15*rate
						}
						values = append(values, []any{from.Add(time.Duration(i) * time.Second).Unix(), fmt.Sprint(value)})
					}
					result = append(result, map[string]any{"metric": map[string]string{"namespace": "tenant-a", "pod": "app-1", "pod_uid": uid}, "values": values})
				}
				json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": result}})
			}))
			reader := TrafficReader{URL: "http://prometheus.invalid", Client: client, Now: func() time.Time { return to.Add(4 * time.Second) }}
			got, err := reader.ReadWindow(context.Background(), Scope{Namespace: "tenant-a", Pods: []PodIdentity{{Name: "app-1", UID: "uid-1"}}}, config, from, to)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Qualified || got.Features["traffic_completions"].Value == nil || *got.Features["traffic_completions"].Value != tc.workRate {
				t.Fatalf("optional denominator changed processed-work accounting: %+v", got)
			}
			if feature := got.Features["traffic_errors"]; feature.Value == nil || math.Abs(*feature.Value-tc.errorRate) > 1e-9 || feature.Unit != "errors/second" {
				t.Fatalf("errors/second lost: %+v", feature)
			}
			fraction := got.Features["traffic_error_rate"]
			if tc.want == nil {
				if fraction.Value != nil || fraction.MissingReason == "" {
					t.Fatalf("unavailable fraction became valid: %+v", fraction)
				}
			} else if fraction.Value == nil || math.Abs(*fraction.Value-*tc.want) > 1e-9 || fraction.Unit != "ratio" {
				t.Fatalf("fraction uses processed work instead of all outcomes: %+v", fraction)
			}
			if tc.omitOutcomes && queriedOutcomes {
				t.Fatal("an unconfigured denominator was queried")
			}
		})
	}
}

func TestOutcomesMetricRejectsArbitraryQueries(t *testing.T) {
	config := testConfig()
	config.Traffic.OutcomesMetric = `rate(other_namespace[1m])`
	if ValidateConfig(config) == nil {
		t.Fatal("outcome denominator accepted arbitrary PromQL")
	}
	config.Traffic.OutcomesMetric = "http_requests_completed_total"
	if err := ValidateConfig(config); err != nil {
		t.Fatal(err)
	}
}

func floatPointer(value float64) *float64 { return &value }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func mockHTTPClient(handler http.Handler) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		return recorder.Result(), nil
	})}
}
