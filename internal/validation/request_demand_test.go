package validation

import (
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"testing"
)

func demandSpec() api.HPAPlusSpec {
	no := false
	age := 120000
	cadence := 30000
	return api.HPAPlusSpec{SyncPeriod: &cadence, IncludeHPA: &no, PredictionFailurePolicy: api.PredictionFailureHoldCurrent, Models: []api.Model{{Name: "request-xgb", Type: api.TypeXGBoost, XGBoost: &api.XGBoost{ForecastStrategy: "direct"}, PredictivePolicy: &api.PredictivePolicy{}, PredictionMaxAge: &age, RequestDemand: &api.RequestDemandConfig{Traffic: api.TrafficProviderConfig{Provider: "prometheus", ArrivalsMetric: "started_total", CompletionsMetric: "finished_total", ArrivalSemantics: "started", CompletionSemantics: "finished", RetrySemantics: "attempts"}}}}}
}
func TestRequestDemandRejectsReactiveFloorsAndAmbiguousTraffic(t *testing.T) {
	for _, mutate := range []func(*api.HPAPlusSpec){
		func(s *api.HPAPlusSpec) { s.SyncPeriod = nil },
		func(s *api.HPAPlusSpec) { cadence := 15000; s.SyncPeriod = &cadence },
		func(s *api.HPAPlusSpec) { yes := true; s.IncludeHPA = &yes },
		func(s *api.HPAPlusSpec) { s.IncludeHPA = nil },
		func(s *api.HPAPlusSpec) { mode := api.ModeAssist; s.Mode = &mode },
		func(s *api.HPAPlusSpec) { s.PredictionFailurePolicy = "" },
		func(s *api.HPAPlusSpec) { s.Models[0].XGBoost.ForecastStrategy = "recursive" },
		func(s *api.HPAPlusSpec) {
			s.Models[0].RequestDemand.Traffic.ArrivalsMetric = s.Models[0].RequestDemand.Traffic.CompletionsMetric
		},
	} {
		s := demandSpec()
		mutate(&s)
		if err := validateRequestDemand(s); err == nil {
			t.Fatal("unsafe request-demand configuration accepted")
		}
	}
	if err := validateRequestDemand(demandSpec()); err != nil {
		t.Fatal(err)
	}
	if err := validateRequestDemand(api.HPAPlusSpec{}); err != nil {
		t.Fatalf("legacy configurations changed: %v", err)
	}
}
