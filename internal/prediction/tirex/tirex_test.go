package tirex_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction/tirex"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type sessionRunner struct {
	sessions []string
	scripts  []string
	requests []map[string]interface{}
	resets   []string
	reply    func(request map[string]interface{}) string
	err      error
}

func (r *sessionRunner) RunSessionWithValue(id, script, value string, timeout int) (string, error) {
	r.sessions, r.scripts = append(r.sessions, id), append(r.scripts, script)
	request := map[string]interface{}{}
	if err := json.Unmarshal([]byte(value), &request); err != nil {
		return "", err
	}
	r.requests = append(r.requests, request)
	if r.err != nil {
		return "", r.err
	}
	return r.reply(request), nil
}

func (r *sessionRunner) ResetSession(id string) error {
	r.resets = append(r.resets, id)
	return nil
}

var start = time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)

func history(n int) []hpaplusv1alpha1.TimestampedSample {
	out := []hpaplusv1alpha1.TimestampedSample{}
	for i := 0; i < n; i++ {
		usage := int64(1000 + 100*i)
		out = append(out, hpaplusv1alpha1.TimestampedSample{Replicas: 2, Time: &metav1.Time{Time: start.Add(time.Duration(30*i) * time.Second)}, TotalCPUUsageMillicores: &usage})
	}
	return out
}

func model() *hpaplusv1alpha1.Model {
	return &hpaplusv1alpha1.Model{Name: "tirex", Type: hpaplusv1alpha1.TypeTiRex, SessionID: "ns/hpa/uid/1/tirex/1/target",
		CPURequestPerPodMillicores: 1000, TargetCPUUtilizationPercentage: 60,
		TiRex: &hpaplusv1alpha1.TiRex{HistorySize: 20, LookAhead: 90000}}
}

// trajectoryReply answers like the worker: three 30 s points after the last sample.
func trajectoryReply(last time.Time, values ...int64) func(map[string]interface{}) string {
	return func(request map[string]interface{}) string {
		points := []string{}
		for i, v := range values {
			at := last.Add(time.Duration(30*(i+1)) * time.Second)
			points = append(points, fmt.Sprintf(`{"at":%q,"until":%q,"cpu":%d}`, at.Format(time.RFC3339Nano), at.Add(30*time.Second).Format(time.RFC3339Nano), v))
		}
		return fmt.Sprintf(`{"protocolVersion":1,"unit":"cpu_millicores","requestId":%q,"ok":true,"prediction":%d,"trajectory":[%s]}`,
			request["requestId"], values[len(values)-1], strings.Join(points, ","))
	}
}

func TestForecastUsesTheModelSessionAndDefaultQuantile(t *testing.T) {
	samples := history(6)
	runner := &sessionRunner{reply: trajectoryReply(samples[5].Time.Time, 1500, 1800, 2400)}
	m := model()
	m.PredictivePolicy = &hpaplusv1alpha1.PredictivePolicy{}
	result, err := (&tirex.Predict{Runner: runner}).GetPredictionResult(m, samples)
	if err != nil {
		t.Fatal(err)
	}
	if result.Replicas != 4 || len(result.Trajectory) != 3 {
		t.Fatalf("2400 millicores at 600 per pod needs 4 pods with a 3-point trajectory: %+v", result)
	}
	if runner.sessions[0] != m.SessionID || runner.scripts[0] != "algorithms/tirex/tirex.py" {
		t.Fatalf("wrong worker session or script: %v %v", runner.sessions, runner.scripts)
	}
	config := runner.requests[0]["config"].(map[string]interface{})
	if config["quantileLevel"] != 0.9 || config["lookAhead"] != float64(90000) || config["returnTrajectory"] != true {
		t.Fatalf("wrong worker config: %v", config)
	}
	if runner.requests[0]["action"] != "forecast" || len(runner.requests[0]["replicaHistory"].([]interface{})) != 6 {
		t.Fatalf("wrong worker request: %v", runner.requests[0])
	}
}

func TestForecastPassesTheConfiguredQuantileAndSkipsSamplesWithoutCPU(t *testing.T) {
	samples := history(4)
	samples = append(samples, hpaplusv1alpha1.TimestampedSample{Replicas: 3, Time: &metav1.Time{Time: start.Add(2 * time.Minute)}})
	runner := &sessionRunner{reply: trajectoryReply(samples[3].Time.Time, 900, 900, 900)}
	m := model()
	q := 0.5
	m.TiRex.QuantileLevel = &q
	if _, err := (&tirex.Predict{Runner: runner}).GetPredictionResult(m, samples); err != nil {
		t.Fatal(err)
	}
	if runner.requests[0]["config"].(map[string]interface{})["quantileLevel"] != 0.5 || len(runner.requests[0]["replicaHistory"].([]interface{})) != 4 {
		t.Fatalf("quantile or CPU history wrong: %v", runner.requests[0])
	}
}

func TestForecastNeedsTwoSamplesAndReportsWorkerFailures(t *testing.T) {
	runner := &sessionRunner{reply: trajectoryReply(start, 1)}
	if _, err := (&tirex.Predict{Runner: runner}).GetPredictionResult(model(), history(1)); !errors.Is(err, prediction.ErrInsufficientHistory) {
		t.Fatalf("one sample must be insufficient history, got %v", err)
	}
	runner.err = errors.New("worker exited")
	if _, err := (&tirex.Predict{Runner: runner}).GetPredictionResult(model(), history(4)); err == nil {
		t.Fatal("worker failure accepted")
	}
	failed := &sessionRunner{reply: func(request map[string]interface{}) string {
		return fmt.Sprintf(`{"protocolVersion":1,"unit":"cpu_millicores","requestId":%q,"ok":false,"error":"bad input"}`, request["requestId"])
	}}
	if _, err := (&tirex.Predict{Runner: failed}).GetPredictionResult(model(), history(4)); err == nil || !strings.Contains(err.Error(), "bad input") {
		t.Fatalf("worker error not reported: %v", err)
	}
	if _, err := (&tirex.Predict{}).GetPredictionResult(model(), history(4)); err == nil {
		t.Fatal("missing persistent runner accepted")
	}
}

func TestSyncSessionsStopsOnlyInactiveWorkersUnderThePrefix(t *testing.T) {
	samples := history(4)
	runner := &sessionRunner{reply: trajectoryReply(samples[3].Time.Time, 1000)}
	p := &tirex.Predict{Runner: runner}
	for _, id := range []string{"ns/a/old", "ns/a/new", "ns/b/other"} {
		m := model()
		m.SessionID = id
		if _, err := p.GetPredictionResult(m, samples); err != nil {
			t.Fatal(err)
		}
	}
	p.SyncSessions("ns/a/", map[string]bool{"ns/a/new": true})
	if len(runner.resets) != 1 || runner.resets[0] != "ns/a/old" {
		t.Fatalf("wrong sessions reset: %v", runner.resets)
	}
}

func TestPruneKeepsTheConfiguredHistory(t *testing.T) {
	m := model()
	m.TiRex.HistorySize = 3
	pruned, err := (&tirex.Predict{}).PruneHistory(m, history(10))
	if err != nil || len(pruned) != 3 || *pruned[2].TotalCPUUsageMillicores != 1900 {
		t.Fatalf("history not pruned to the newest 3 samples: %v %v", pruned, err)
	}
}
