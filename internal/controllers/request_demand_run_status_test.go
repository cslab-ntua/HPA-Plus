package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/observability"
	requestforecast "github.com/cslab-ntua/HPA-Plus/internal/prediction/requestdemand"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRequestForecastAttemptReportsItsOwnRunInsteadOfOldCPUFit(t *testing.T) {
	for _, tc := range []struct {
		name, status, outcome string
		err                   error
	}{
		{"ready", "ready", "success", nil},
		{"warming", "warming", "insufficient_history", nil},
		{"unavailable", "unavailable", "failed", nil},
		{"protocol-error", "", "failed", fmt.Errorf("bad worker reply")},
		{"timeout", "", "timeout", context.DeadlineExceeded},
		{"canceled", "", "canceled", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requested := time.Date(2026, 10, 20, 17, 0, 4, 0, time.UTC)
			now := requested
			registry := prometheus.NewRegistry()
			metrics, err := observability.New(registry)
			if err != nil {
				t.Fatal(err)
			}
			r := &HPAPlusReconciler{Now: func() time.Time { return now }, Telemetry: metrics}
			old := metav1.NewTime(requested.Add(-5 * time.Minute))
			oldDuration := int64(3430)
			status := &api.ModelStatus{LastAttemptTime: old.DeepCopy(), LastSuccessTime: old.DeepCopy(), LastRunDurationMilliseconds: &oldDuration, LastRunOutcome: "success"}
			calls := 0
			r.WorkloadRunner = demandIntegrationRunner(func(_, payload string, _ int) (string, error) {
				calls++
				now = now.Add(2 * time.Second)
				if tc.err != nil {
					return "", tc.err
				}
				var input struct{ RequestedAt, FirstForecastAt time.Time }
				if err := json.Unmarshal([]byte(payload), &input); err != nil {
					return "", err
				}
				result := requestforecast.Result{Protocol: requestforecast.Protocol, Unit: requestforecast.Unit, GeneratedAt: input.RequestedAt, Status: tc.status, Forecasts: []requestforecast.Point{}}
				if tc.status == "ready" {
					result.TrainedThrough = requested.Truncate(30 * time.Second)
					for i := 0; i < 3; i++ {
						at := input.FirstForecastAt.Add(time.Duration(i) * 30 * time.Second)
						result.Forecasts = append(result.Forecasts, requestforecast.Point{At: at, Until: at.Add(30 * time.Second), Value: 20})
					}
				} else {
					result.Reason = "not enough eligible observations"
				}
				body, err := json.Marshal(result)
				return string(body), err
			})
			config := requestforecast.Defaults()
			var rows []requestforecast.Observation
			for i := 0; i < 60; i++ {
				at := requested.Truncate(30 * time.Second).Add(time.Duration(i-60) * 30 * time.Second)
				value := float64(i%7 + 20)
				rows = append(rows, requestforecast.Observation{At: at, Until: at.Add(30 * time.Second), ReceivedAt: at.Add(34 * time.Second), ArrivalsPerSecond: &value})
			}
			result, runErr := r.runRequestForecast(context.Background(), &api.Model{Type: api.TypeXGBoost, XGBoost: &api.XGBoost{HistorySize: 60, Lags: 12, LookAhead: 90000}}, rows, requested, requested.Truncate(30*time.Second).Add(30*time.Second), config, status)
			if tc.err == nil && runErr != nil {
				t.Fatal(runErr)
			}
			if calls != 1 {
				t.Fatalf("invocation count %d", calls)
			}
			if status.LastRunOutcome != tc.outcome || !status.LastAttemptTime.Time.Equal(requested) || status.LastRunDurationMilliseconds == nil || *status.LastRunDurationMilliseconds == oldDuration {
				t.Fatalf("old CPU-fit metadata retained: %+v", status)
			}
			if tc.outcome == "success" {
				if !status.LastSuccessTime.Time.Equal(now) || result.Status != "ready" {
					t.Fatal("request success timestamp not updated")
				}
			} else if !status.LastSuccessTime.Equal(&old) {
				t.Fatal("failed/warming call erased the preceding successful fit")
			}
			families, err := registry.Gather()
			if err != nil {
				t.Fatal(err)
			}
			count := 0.
			for _, family := range families {
				if family.GetName() != "hpaplus_model_runs_total" {
					continue
				}
				for _, metric := range family.Metric {
					match := false
					for _, label := range metric.Label {
						if label.GetName() == "outcome" && label.GetValue() == tc.outcome {
							match = true
						}
					}
					if match {
						count += metric.GetCounter().GetValue()
					}
				}
			}
			if count != 1 {
				t.Fatalf("request attempt missing or double-counted in model telemetry: %v", count)
			}
		})
	}
}
