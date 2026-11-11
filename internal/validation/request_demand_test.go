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
		func(s *api.HPAPlusSpec) { s.Models[0].PredictivePolicy = nil },
		func(s *api.HPAPlusSpec) { s.Models[0].PredictionMaxAge = nil },
		func(s *api.HPAPlusSpec) { s.Models[0].RequestDemand.ForecastTarget = "arrivals" },
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

func TestRequestDemandAcceptsEveryModelType(t *testing.T) {
	for _, model := range []api.Model{
		{Type: api.TypeLinear, Linear: &api.Linear{}},
		{Type: api.TypeHoltWinters, HoltWinters: &api.HoltWinters{}},
		{Type: api.TypeArima, Arima: &api.Arima{}},
		{Type: api.TypeLightGBM, LightGBM: &api.LightGBM{}},
		{Type: api.TypeXGBoost, XGBoost: &api.XGBoost{ForecastStrategy: "recursive"}},
	} {
		s := demandSpec()
		model.Name, model.PredictivePolicy, model.PredictionMaxAge, model.RequestDemand = "m", s.Models[0].PredictivePolicy, s.Models[0].PredictionMaxAge, s.Models[0].RequestDemand
		s.Models = []api.Model{model}
		if err := validateRequestDemand(s); err != nil {
			t.Fatalf("%s: %v", model.Type, err)
		}
	}
}
