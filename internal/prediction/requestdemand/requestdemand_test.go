package requestdemand

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

func TestConfiguredForecastHorizon(t *testing.T) {
	for _, example := range []struct {
		name      string
		lookAhead int
		steps     int
		invalid   bool
	}{
		{"ninety seconds", 90000, 3, false},
		{"five minutes", 300000, 10, false},
		{"fractional interval rounds up", 90001, 4, false},
		{"maximum supported", 600000, 20, false},
		{"unsupported long horizon", 600001, 0, true},
		{"missing horizon", 0, 0, true},
		{"negative horizon", -1, 0, true},
	} {
		t.Run(example.name, func(t *testing.T) {
			config, err := ConfigFromModel(&api.Model{XGBoost: &api.XGBoost{LookAhead: example.lookAhead}})
			if example.invalid {
				if err == nil {
					t.Fatal("accepted unsupported request horizon")
				}
				return
			}
			if err != nil || config.HorizonSteps != example.steps || config.IntervalSeconds != 30 {
				t.Fatalf("got %+v error=%v; wanted %d intervals", config, err, example.steps)
			}
		})
	}
}

type fakeRunner struct {
	called bool
	reply  string
	input  request
}

func (f *fakeRunner) RunAlgorithmWithValue(script, input string, timeout int) (string, error) {
	f.called = true
	if err := json.Unmarshal([]byte(input), &f.input); err != nil {
		return "", err
	}
	return f.reply, nil
}

func TestWarmupUsesTypedProtocol(t *testing.T) {
	now := time.Unix(1800001802, 0).UTC()
	f := &fakeRunner{reply: `{"protocol":"request-demand-v1","unit":"requests_per_second","generatedAt":"2027-01-15T08:30:02Z","status":"warming","reason":"need 60 genuine arrival samples; have 0","forecasts":[]}`}
	// Use generated timestamps rather than relying on an epoch/date conversion.
	raw, _ := json.Marshal(Result{Protocol: Protocol, Unit: Unit, GeneratedAt: now, Status: "warming", Reason: "need history", Forecasts: []Point{}})
	f.reply = string(raw)
	result, err := Forecast(context.Background(), f, Defaults(), nil, now, now.Truncate(30*time.Second).Add(time.Minute))
	if err != nil || result.Status != "warming" || !f.called {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if f.input.Protocol != Protocol || f.input.Config.NEstimators != 300 || f.input.Config.Quantile != .95 {
		t.Fatalf("wrong explicit model contract: %+v", f.input)
	}
}

func TestRejectsNullValueAndMisalignedGrid(t *testing.T) {
	now := time.Unix(1800001802, 0).UTC()
	first := now.Truncate(30 * time.Second).Add(time.Minute)
	result := Result{Protocol: Protocol, Unit: Unit, GeneratedAt: now, Status: "ready", TrainedThrough: now.Add(-2 * time.Second)}
	for i := 0; i < 3; i++ {
		at := first.Add(time.Duration(i) * 30 * time.Second)
		result.Forecasts = append(result.Forecasts, Point{At: at, Until: at.Add(30 * time.Second), Value: 0})
	}
	raw, _ := json.Marshal(result)
	if _, err := decode(string(raw), Defaults(), now, first); err != nil {
		t.Fatal(err)
	}
	if _, err := decode(strings.Replace(string(raw), `"value":0`, `"value":null`, 1), Defaults(), now, first); err == nil {
		t.Fatal("accepted null as a measured zero")
	}
	if _, err := decode(string(raw), Defaults(), now, first.Add(time.Second)); err == nil {
		t.Fatal("accepted wrong forecast interval")
	}
}

func TestContextCancellationAndObservationValidation(t *testing.T) {
	now := time.Unix(1800001802, 0).UTC()
	f := &fakeRunner{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Forecast(ctx, f, Defaults(), nil, now, now.Truncate(30*time.Second).Add(time.Minute))
	if err == nil || f.called {
		t.Fatal("canceled request invoked worker")
	}
	observation := Observation{At: now.Truncate(30 * time.Second), Until: now.Truncate(30 * time.Second).Add(30 * time.Second), ReceivedAt: now}
	_, err = Forecast(context.Background(), f, Defaults(), []Observation{observation}, now, now.Truncate(30*time.Second).Add(time.Minute))
	if err == nil || f.called {
		t.Fatal("incomplete interval invoked worker")
	}
}
