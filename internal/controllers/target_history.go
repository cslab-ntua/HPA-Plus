package controllers

import (
	"context"
	"fmt"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/history"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
)

func scaleTargetIdentity(instance *api.HPAPlus, scale *autoscalingv1.Scale) string {
	return history.TargetIdentity(instance.Spec.ScaleTargetRef, scale.UID)
}

func (r *HPAPlusReconciler) ensureTargetHistory(ctx context.Context, instance *api.HPAPlus, scale *autoscalingv1.Scale) (*api.HPAPlusData, error) {
	if scale.UID == "" {
		return nil, fmt.Errorf("scale target has no workload UID")
	}
	identity := scaleTargetIdentity(instance, scale)
	source := "legacy-injected"
	if provider, ok := r.Gatherer.(interface{ CPUHistorySourceIdentity() string }); ok {
		source = provider.CPUHistorySourceIdentity()
	}
	var result *api.HPAPlusData
	err := r.mutateConfigMapData(ctx, instance, func(data *api.HPAPlusData) (bool, error) {
		result = data
		if data.TargetIdentity == identity && data.CPUHistorySource == source {
			return false, nil
		}
		// Legacy data has no provable workload identity. Warm up again once rather
		// than transferring an old workload's demand to an unrelated replacement.
		data.TargetIdentity = identity
		data.CPUHistorySource = source
		data.ModelHistories = map[string]api.ModelHistory{}
		// PendingScale has independent ownership/UID checks and must survive this reset.
		return true, nil
	})
	return result, err
}

func (r *HPAPlusReconciler) syncTargetStatus(ctx context.Context, instance *api.HPAPlus, scale *autoscalingv1.Scale, identity string) error {
	if instance.Status.TargetIdentity == identity {
		return nil
	}
	return r.mutateStatus(ctx, instance, func(status *api.HPAPlusStatus) {
		// Preserve existing rate accounting during first adoption of legacy status.
		// Once bound, histories must never migrate to another resolved workload.
		if status.TargetIdentity != "" && status.TargetIdentity != identity {
			status.ScaleUpEventHistory = nil
			status.ScaleDownEventHistory = nil
			status.ScaleUpReplicaHistory = nil
			status.ScaleDownReplicaHistory = nil
			status.LastScaleTime = nil
			status.LastScaleDecisionID = ""
		}
		status.TargetIdentity = identity
		status.DesiredReplicas, status.CurrentReplicas = scale.Spec.Replicas, scale.Status.Replicas
	})
}
