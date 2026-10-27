package requestdemand

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

func TestIntegratedFeatureKeepsNativeReadAfterMeasurementBoundary(t *testing.T) {
	end := time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)
	now := end.Add(4 * time.Second)
	value := 1.2
	feature := FeatureValue{Value: &value, SourceAt: end.Add(2*time.Second + 123*time.Nanosecond), ReceivedAt: now, Unit: "cores",
		MeasurementWindow: &MeasurementWindow{From: end.Add(-30 * time.Second), To: end}}
	observation := Observation{At: end.Add(-30 * time.Second), Until: end, ReceivedAt: now, Features: map[string]FeatureValue{"cpu": feature}}
	c := Defaults()
	c.Features = []FeatureSpec{{Name: "cpu", Unit: "cores", MaxAgeSeconds: 90}}
	raw, _ := json.Marshal(Result{Protocol: Protocol, Unit: Unit, GeneratedAt: now, Status: "warming", Reason: "need history", Forecasts: []Point{}})
	runner := &fakeRunner{reply: string(raw)}
	if _, err := Forecast(context.Background(), runner, c, []Observation{observation}, now, end.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	actual := runner.input.Observations[0].Features["cpu"]
	if !actual.SourceAt.Equal(feature.SourceAt) || !actual.ReceivedAt.Equal(now) || actual.MeasurementWindow == nil || !actual.MeasurementWindow.To.Equal(end) {
		t.Fatalf("native timestamps/window were lost or backdated: %+v", actual)
	}
	for _, tc := range []struct {
		name   string
		change func(*FeatureValue)
	}{
		{"instant future source", func(f *FeatureValue) { f.MeasurementWindow = nil }},
		{"future measured window", func(f *FeatureValue) { f.MeasurementWindow.To = end.Add(time.Second) }},
		{"empty measured window", func(f *FeatureValue) { f.MeasurementWindow.From = end }},
		{"receipt before read", func(f *FeatureValue) { f.ReceivedAt = end.Add(time.Second) }},
		{"receipt before measured end", func(f *FeatureValue) { f.SourceAt = end.Add(-2 * time.Second); f.ReceivedAt = end.Add(-time.Second) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := feature
			window := *feature.MeasurementWindow
			copy.MeasurementWindow = &window
			tc.change(&copy)
			observation.Features["cpu"] = copy
			if err := validate(c, []Observation{observation}, now, end.Add(30*time.Second)); err == nil {
				t.Fatal("invalid feature timing accepted")
			}
		})
	}
	// A legitimately late feature remains representable. The Python matrix masks
	// it at earlier cutoffs instead of changing its availability timestamp.
	feature.ReceivedAt = now.Add(12 * time.Second)
	observation.Features["cpu"] = feature
	if err := validate(c, []Observation{observation}, now, end.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestArrivalCoverageRequiresQualifiedGenuineLabels(t *testing.T) {
	end := time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)
	value, coverage := 0., .94
	row := Observation{At: end.Add(-30 * time.Second), Until: end, ReceivedAt: end.Add(4 * time.Second), ArrivalsPerSecond: &value, ArrivalCoverage: &coverage, ArrivalBoundaryAveraged: true}
	if err := validate(Defaults(), []Observation{row}, row.ReceivedAt, end.Add(30*time.Second)); err == nil {
		t.Fatal("partial label counted as a genuine zero")
	}
	coverage = .95
	if err := validate(Defaults(), []Observation{row}, row.ReceivedAt, end.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	coverage = .1
	row.ArrivalsPerSecond = nil
	if err := validate(Defaults(), []Observation{row}, row.ReceivedAt, end.Add(30*time.Second)); err != nil {
		t.Fatal("honest missing sample was rejected", err)
	}
}

func TestAllPublicRequestXGBoostParametersReachWorker(t *testing.T) {
	lr, sub, col, child, gamma, lambda, alpha := .03, .9, .7, 0., .2, 2.5, .4
	objective := "reg:quantileerror"
	model := &api.Model{XGBoost: &api.XGBoost{LookAhead: 90000, LearningRate: &lr, Subsample: &sub, ColsampleBytree: &col, MinChildWeight: &child, Gamma: &gamma, RegLambda: &lambda, RegAlpha: &alpha, Objective: &objective}}
	c, err := ConfigFromModel(model)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 17, 0, 4, 0, time.UTC)
	raw, _ := json.Marshal(Result{Protocol: Protocol, Unit: Unit, GeneratedAt: now, Status: "warming", Reason: "need history", Forecasts: []Point{}})
	runner := &fakeRunner{reply: string(raw)}
	if _, err := Forecast(context.Background(), runner, c, nil, now, now.Truncate(30*time.Second).Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	actual := runner.input.Config
	if actual.LearningRate != lr || actual.Subsample != sub || actual.ColsampleBytree != col || actual.MinChildWeight != 0 || actual.Gamma != gamma || actual.RegLambda != lambda || actual.RegAlpha != alpha {
		t.Fatalf("configured parameters ignored: %+v", actual)
	}
	for _, bad := range []float64{0, -1, 1.01, math.NaN(), math.Inf(1)} {
		model.XGBoost.LearningRate = &bad
		if _, err := ConfigFromModel(model); err == nil {
			t.Fatalf("invalid learningRate accepted: %v", bad)
		}
	}
	model.XGBoost.LearningRate = &lr
	bad := 1e6 + 1
	model.XGBoost.Gamma = &bad
	if _, err := ConfigFromModel(model); err == nil {
		t.Fatal("unbounded regularization accepted")
	}
	model.XGBoost.Gamma = &gamma
	objective = "survival:aft"
	if _, err := ConfigFromModel(model); err == nil {
		t.Fatal("request mode silently changed an AFT objective")
	}
}

func TestAvailabilityDiagnosticsCannotClaimImpossibleInputCounts(t *testing.T) {
	c := Defaults()
	c.Features = []FeatureSpec{{Name: "cpu", Unit: "cores", MaxAgeSeconds: 90}}
	d := Diagnostics{TrainingRows: 20, FeatureAvailability: map[string]FeatureAvailability{"cpu": {TrainingTotal: 240, TrainingNonMissing: 120, InferenceTotal: 36, InferenceNonMissing: 0, TrainingRowsWithValue: 20}}}
	if err := validateDiagnostics(d, c); err != nil {
		t.Fatal(err)
	}
	v := d.FeatureAvailability["cpu"]
	v.InferenceNonMissing = 37
	d.FeatureAvailability["cpu"] = v
	if err := validateDiagnostics(d, c); err == nil {
		t.Fatal("impossible nonmissing matrix count accepted")
	}
}
