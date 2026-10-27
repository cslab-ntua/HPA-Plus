package v1alpha1

import (
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
)

func TestDefaultScaleUpIsTheKubernetesRule(t *testing.T) {
	up := DefaultBehavior(nil).ScaleUp
	if *up.StabilizationWindowSeconds != 0 || *up.SelectPolicy != autoscalingv2.MaxChangePolicySelect || len(up.Policies) != 2 {
		t.Fatalf("unexpected default scale-up rules: %+v", up)
	}
	for _, policy := range up.Policies {
		if policy.PeriodSeconds != 15 {
			t.Fatalf("scale-up policy %s has a %d s period, want Kubernetes' 15 s", policy.Type, policy.PeriodSeconds)
		}
	}
	if up.Policies[0].Type != autoscalingv2.PercentScalingPolicy || up.Policies[0].Value != 100 ||
		up.Policies[1].Type != autoscalingv2.PodsScalingPolicy || up.Policies[1].Value != 4 {
		t.Fatalf("default scale-up is not double or +4 pods: %+v", up.Policies)
	}
}

func TestExplicitScaleUpRulesOverrideTheDefault(t *testing.T) {
	period := []autoscalingv2.HPAScalingPolicy{{Type: autoscalingv2.PodsScalingPolicy, Value: 8, PeriodSeconds: 60}}
	behavior := DefaultBehavior(&autoscalingv2.HorizontalPodAutoscalerBehavior{ScaleUp: &autoscalingv2.HPAScalingRules{Policies: period}})
	if len(behavior.ScaleUp.Policies) != 1 || behavior.ScaleUp.Policies[0].PeriodSeconds != 60 || behavior.ScaleUp.Policies[0].Value != 8 {
		t.Fatalf("explicit scale-up rules were replaced: %+v", behavior.ScaleUp.Policies)
	}
}
