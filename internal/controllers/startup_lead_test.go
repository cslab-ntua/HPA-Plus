package controllers

import (
	"testing"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestScaleUpLeadFollowsObservedPodStartup(t *testing.T) {
	created := time.Date(2026, 10, 21, 19, 0, 0, 0, time.UTC)
	pod := func(ready time.Duration, status corev1.ConditionStatus) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(created)},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status, LastTransitionTime: metav1.NewTime(created.Add(ready))}}}}
	}
	deleting := pod(20*time.Second, corev1.ConditionTrue)
	now := metav1.NewTime(created)
	deleting.DeletionTimestamp = &now
	// r14: 102 pods became ready 2-3 s after creation.
	pods := []corev1.Pod{pod(2*time.Second, corev1.ConditionTrue), pod(3*time.Second, corev1.ConditionTrue),
		pod(time.Hour, corev1.ConditionTrue), pod(40*time.Second, corev1.ConditionFalse), deleting}
	if startup, ok := observedStartup(pods); !ok || startup != 3*time.Second {
		t.Fatalf("observed startup %v %v, want 3s", startup, ok)
	}
	policy := api.PredictivePolicy{ScaleUpLeadSeconds: 30, ScaleUpLeadFromStartup: true}
	if got := effectivePolicy(policy, pods); got.ScaleUpLeadSeconds != 8 {
		t.Fatalf("lead %d, want startup 3 s + 5 s", got.ScaleUpLeadSeconds)
	}
	if got := effectivePolicy(policy, nil); got.ScaleUpLeadSeconds != 30 {
		t.Fatal("without an observed start the configured lead must stay")
	}
	policy.ScaleUpLeadFromStartup = false
	if got := effectivePolicy(policy, pods); got.ScaleUpLeadSeconds != 30 {
		t.Fatal("the configured lead changed without opting in")
	}
	model := &api.Model{PredictivePolicy: &api.PredictivePolicy{ScaleUpLeadSeconds: 30, ScaleUpLeadFromStartup: true}}
	if scoped := scopedPolicy(model, pods); scoped.PredictivePolicy.ScaleUpLeadSeconds != 8 || model.PredictivePolicy.ScaleUpLeadSeconds != 30 {
		t.Fatal("scoped policy must not modify the model spec")
	}
}
