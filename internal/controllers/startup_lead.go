package controllers

import (
	"math"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// startupLeadMargin covers the decision and the scale write before a new pod
// is created; the measured part runs from pod creation to readiness.
const startupLeadMargin = 5 * time.Second

// A pod that became ready this long after its creation changed readiness
// later in its life; that is not a startup time.
const maximumObservedStartup = 10 * time.Minute

// observedStartup is the longest time a current pod took from creation to
// becoming ready.
func observedStartup(pods []corev1.Pod) (time.Duration, bool) {
	longest, found := time.Duration(0), false
	for _, p := range pods {
		if p.DeletionTimestamp != nil {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type != corev1.PodReady || c.Status != corev1.ConditionTrue {
				continue
			}
			startup := c.LastTransitionTime.Sub(p.CreationTimestamp.Time)
			if startup < 0 || startup > maximumObservedStartup {
				continue
			}
			if !found || startup > longest {
				longest, found = startup, true
			}
		}
	}
	return longest, found
}

// effectivePolicy applies scaleUpLeadFromStartup: the lead becomes the observed
// startup plus margin. Without an observed start the configured lead stays.
func effectivePolicy(policy api.PredictivePolicy, pods []corev1.Pod) api.PredictivePolicy {
	if !policy.ScaleUpLeadFromStartup {
		return policy
	}
	if startup, ok := observedStartup(pods); ok {
		policy.ScaleUpLeadSeconds = int32(math.Ceil((startup + startupLeadMargin).Seconds()))
	}
	return policy
}

// scopedPolicy is the model with its effective predictive policy.
func scopedPolicy(model *api.Model, pods []corev1.Pod) *api.Model {
	if model.PredictivePolicy == nil {
		return model
	}
	scoped := *model
	policy := effectivePolicy(*model.PredictivePolicy, pods)
	scoped.PredictivePolicy = &policy
	return &scoped
}
