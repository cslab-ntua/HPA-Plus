package requestdemand

import (
	"encoding/json"
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"testing"
	"time"
)

func TestIndependentHistoryAndForecastIntervals(t *testing.T) {
	model := &api.Model{XGBoost: &api.XGBoost{LookAhead: 90000}, RequestDemand: &api.RequestDemandConfig{ForecastIntervalSeconds: 10}}
	config, err := ConfigFromModel(model)
	if err != nil || config.IntervalSeconds != 30 || config.ForecastIntervalSeconds != 10 || config.HorizonSteps != 9 {
		t.Fatalf("history cadence or physical horizon changed: %+v %v", config, err)
	}
	now := time.Unix(1800001802, 0).UTC()
	first := now.Truncate(10 * time.Second).Add(10 * time.Second)
	if err = validate(config, nil, now, first); err != nil {
		t.Fatal(err)
	}
	result := Result{Protocol: Protocol, Unit: Unit, GeneratedAt: now, Status: "ready", TrainedThrough: now.Add(-2 * time.Second), Diagnostics: Diagnostics{ForecastIntervalSeconds: 10}}
	for i := 0; i < 9; i++ {
		at := first.Add(time.Duration(i) * 10 * time.Second)
		result.Forecasts = append(result.Forecasts, Point{At: at, Until: at.Add(10 * time.Second), Value: 3})
	}
	raw, _ := json.Marshal(result)
	if _, err = decode(string(raw), config, now, first); err != nil {
		t.Fatal(err)
	}
	result.Forecasts[0].Until = result.Forecasts[0].Until.Add(time.Second)
	raw, _ = json.Marshal(result)
	if _, err = decode(string(raw), config, now, first); err == nil {
		t.Fatal("accepted a wrong applicability interval")
	}
	model.RequestDemand.ForecastIntervalSeconds = 7
	if _, err = ConfigFromModel(model); err == nil {
		t.Fatal("accepted unsupported interval")
	}
}

func TestSubintervalValidationPreservesMissingAndHonestAvailability(t *testing.T) {
	config := Defaults()
	config.ForecastIntervalSeconds = 10
	config.HorizonSteps = 9
	start := time.Unix(1800000000, 0).UTC()
	receipt := start.Add(34 * time.Second)
	first := start.Add(40 * time.Second)
	observation := Observation{At: start, Until: start.Add(30 * time.Second), ReceivedAt: receipt, ArrivalIntervals: []ArrivalInterval{{At: start, Until: start.Add(10 * time.Second), ReceivedAt: receipt, Coverage: .9}}}
	if err := validate(config, []Observation{observation}, receipt, first); err != nil {
		t.Fatal(err)
	}
	zero := 0.
	observation.ArrivalIntervals[0].Rate = &zero
	if err := validate(config, []Observation{observation}, receipt, first); err == nil {
		t.Fatal("missing subinterval changed to a genuine zero")
	}
	observation.ArrivalIntervals[0].Rate = nil
	observation.ArrivalIntervals[0].ReceivedAt = receipt.Add(time.Nanosecond)
	if err := validate(config, []Observation{observation}, receipt, first); err == nil {
		t.Fatal("accepted a backdated observation envelope")
	}
}
