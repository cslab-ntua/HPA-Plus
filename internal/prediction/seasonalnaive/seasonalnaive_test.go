package seasonalnaive_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/fake"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction/seasonalnaive"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func history(n int) []hpaplusv1alpha1.TimestampedSample {
	start := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	out := []hpaplusv1alpha1.TimestampedSample{}
	for i := 0; i < n; i++ {
		usage := int64(1000 + 100*i)
		out = append(out, hpaplusv1alpha1.TimestampedSample{Replicas: 2, Time: &metav1.Time{Time: start.Add(time.Duration(30*i) * time.Second)}, TotalCPUUsageMillicores: &usage})
	}
	return out
}

func model() *hpaplusv1alpha1.Model {
	return &hpaplusv1alpha1.Model{Name: "naive", Type: hpaplusv1alpha1.TypeSeasonalNaive, CPURequestPerPodMillicores: 1000, TargetCPUUtilizationPercentage: 60,
		SeasonalNaive: &hpaplusv1alpha1.SeasonalNaive{HistorySize: 20, LookAhead: 90000}}
}

func TestForecastSendsHistoryAndOptionalCycle(t *testing.T) {
	var payload map[string]interface{}
	var script string
	runner := &fake.Run{RunAlgorithmWithValueReactor: func(path, value string, timeout int) (string, error) {
		script = path
		if err := json.Unmarshal([]byte(value), &payload); err != nil {
			return "", err
		}
		return "2400", nil
	}}
	m := model()
	cycle := 300
	m.SeasonalNaive.SeasonalPeriodSeconds = &cycle
	result, err := (&seasonalnaive.Predict{Runner: runner}).GetPredictionResult(m, history(6))
	if err != nil {
		t.Fatal(err)
	}
	if result.Replicas != 4 || script != "algorithms/seasonal_naive/seasonal_naive.py" {
		t.Fatalf("2400 millicores at 600 per pod needs 4 pods: %+v %s", result, script)
	}
	if payload["lookAhead"] != float64(90000) || payload["seasonalPeriodSeconds"] != float64(300) || len(payload["replicaHistory"].([]interface{})) != 6 {
		t.Fatalf("wrong payload: %v", payload)
	}
}

func TestForecastNeedsThreeSamplesAndReportsFailures(t *testing.T) {
	runner := &fake.Run{RunAlgorithmWithValueReactor: func(string, string, int) (string, error) { return "", errors.New("worker failed") }}
	if _, err := (&seasonalnaive.Predict{Runner: runner}).GetPredictionResult(model(), history(2)); !errors.Is(err, prediction.ErrInsufficientHistory) {
		t.Fatalf("two samples must be insufficient history, got %v", err)
	}
	if _, err := (&seasonalnaive.Predict{Runner: runner}).GetPredictionResult(model(), history(5)); err == nil {
		t.Fatal("worker failure accepted")
	}
}

func TestPruneKeepsTheConfiguredHistory(t *testing.T) {
	m := model()
	m.SeasonalNaive.HistorySize = 4
	pruned, err := (&seasonalnaive.Predict{}).PruneHistory(m, history(10))
	if err != nil || len(pruned) != 4 {
		t.Fatalf("history not pruned to 4 samples: %d %v", len(pruned), err)
	}
}
