package decision

import (
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"testing"
	"time"
)

func TestExplicitHoldDoesNotUseReactiveRecommendation(t *testing.T) {
	for _, baseline := range []int32{1, 8} {
		in := Input{Spec: api.HPAPlusSpec{MaxReplicas: 8, PredictionFailurePolicy: api.PredictionFailureHoldCurrent}, Now: time.Now(), Requested: 3, Baseline: baseline}
		p, err := Plan(in)
		if err != nil {
			t.Fatal(err)
		}
		if p.Desired != 3 || p.BaselineIncluded || p.Mode != "hold" {
			t.Fatalf("baseline %d leaked into hold: %+v", baseline, p)
		}
		comparison := p.BaselineDesired
		in.Spec.PredictionFailurePolicy = ""
		p, err = Plan(in)
		if err != nil || p.Desired != comparison || !p.BaselineIncluded {
			t.Fatalf("legacy fallback changed: %+v %v", p, err)
		}
	}
}

func TestRequestModeHasNoImplicitReactiveFallback(t *testing.T) {
	in := Input{Spec: api.HPAPlusSpec{MaxReplicas: 8, Models: []api.Model{{RequestDemand: &api.RequestDemandConfig{}}}}, Now: time.Now(), Requested: 4, Baseline: 8}
	p, err := Plan(in)
	if err != nil || p.Desired != 4 || p.BaselineIncluded {
		t.Fatalf("request warmup used reactive: %+v %v", p, err)
	}
	in.Predictions = []int32{2}
	p, err = Plan(in)
	if err != nil || p.Desired != 2 || p.BaselineIncluded {
		t.Fatalf("predictive recommendation was floored: %+v %v", p, err)
	}
}
