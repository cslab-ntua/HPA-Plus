package controllers

import (
	"context"
	"errors"
	"fmt"
	"github.com/cslab-ntua/HPA-Plus/internal/history"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/observability"
	"github.com/cslab-ntua/HPA-Plus/internal/scalebehavior"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var errMetricsExpired = errors.New("metrics expired before write; gather again")
var errPredictionsExpired = errors.New("prediction inputs expired before write; evaluate again")

type decisionValidity struct {
	metricsUntil     *time.Time
	predictionsUntil *time.Time
}

func (v decisionValidity) check(now time.Time) error {
	// Source ages permit equality; prediction cache expiry is exclusive.
	if v.metricsUntil != nil && now.After(*v.metricsUntil) {
		return errMetricsExpired
	}
	if v.predictionsUntil != nil && !now.Before(*v.predictionsUntil) {
		return errPredictionsExpired
	}
	return nil
}

func (r *HPAPlusReconciler) clearScaleDecision(ctx context.Context, instance *api.HPAPlus, id string) error {
	return r.mutateConfigMapData(ctx, instance, func(data *api.HPAPlusData) (bool, error) {
		if data.PendingScale == nil || data.PendingScale.ID != id {
			return false, nil
		}
		data.PendingScale = nil
		return true, nil
	})
}

func (r *HPAPlusReconciler) applyScaleDecision(ctx context.Context, instance *api.HPAPlus, scale *autoscalingv1.Scale,
	resource schema.GroupResource, desired int32, up, down []api.TimestampedSample, validity decisionValidity) error {
	if instance.Spec.EffectiveMode() == api.ModeObserve {
		return fmt.Errorf("Scale writes are disabled in observe mode")
	}
	trace, _ := ctx.Value(decisionTraceKey{}).(*decisionTrace)
	if trace == nil {
		ctx, trace = decisionContext(ctx, string(uuid.NewUUID()))
	}
	// A model can run for seconds. Do not apply its result to a changed/deleting resource.
	latest := &api.HPAPlus{}
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(instance), latest); err != nil {
		return err
	}
	if latest.UID != instance.UID || latest.Generation != instance.Generation || latest.DeletionTimestamp != nil {
		return fmt.Errorf("HPAPlus changed while evaluating decision")
	}
	if err := validity.check(r.now()); err != nil {
		return err
	}
	if scale.Spec.Replicas == desired {
		trace.write = "unchanged"
		return r.mutateStatusChecked(ctx, instance, func(status *api.HPAPlusStatus) error {
			if err := validity.check(r.now()); err != nil {
				return err
			}
			before := status.DeepCopy()
			status.DesiredReplicas, status.CurrentReplicas = desired, scale.Status.Replicas
			status.ScaleUpReplicaHistory, status.ScaleDownReplicaHistory = up, down
			err := checkAccountingBudget(before, status)
			if err != nil {
				r.Telemetry.ObserveStatusRejection("accounting")
			}
			return err
		})
	}
	decision := &api.ScaleDecision{ID: trace.id, OwnerUID: string(instance.UID),
		Target: instance.Spec.ScaleTargetRef, TargetUID: string(scale.UID), ResourceVersion: scale.ResourceVersion,
		PreviousDesired: scale.Spec.Replicas, Desired: desired, Observed: scale.Status.Replicas,
		Time: metav1.Time{Time: r.now()}, ScaleUpRecommendations: up, ScaleDownRecommendations: down}
	if err := r.checkDecisionStatusBudget(latest, decision, desired, scale.Status.Replicas); err != nil {
		return err
	}
	if err := r.mutateConfigMapData(ctx, instance, func(data *api.HPAPlusData) (bool, error) {
		if err := validity.check(r.now()); err != nil {
			return false, err
		}
		if data.PendingScale != nil {
			return false, fmt.Errorf("unresolved scale decision %s", data.PendingScale.ID)
		}
		data.PendingScale = decision
		return true, nil
	}); err != nil {
		return err
	}
	if expired := validity.check(r.now()); expired != nil {
		if err := r.clearScaleDecision(ctx, instance, decision.ID); err != nil {
			return fmt.Errorf("%w; clear journal: %v", expired, err)
		}
		return expired
	}
	// Staging a multi-object history snapshot can take time. Recheck owner
	// identity after publication as well as before evaluating the decision.
	currentOwner := &api.HPAPlus{}
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(instance), currentOwner); err != nil {
		return err
	}
	if currentOwner.UID != instance.UID || currentOwner.Generation != instance.Generation || currentOwner.DeletionTimestamp != nil {
		if currentOwner.UID == instance.UID && currentOwner.DeletionTimestamp == nil {
			_ = r.clearScaleDecision(ctx, currentOwner, decision.ID)
		}
		return fmt.Errorf("HPAPlus changed before Scale write")
	}
	if err := r.checkDecisionStatusBudget(currentOwner, decision, desired, scale.Status.Replicas); err != nil {
		if clearErr := r.clearScaleDecision(ctx, instance, decision.ID); clearErr != nil {
			return fmt.Errorf("%w; clear journal: %v", err, clearErr)
		}
		return err
	}
	updated := scale.DeepCopy()
	updated.Spec.Replicas = desired
	trace.write = "unknown"
	if _, err := r.ScaleClient.Scales(instance.Namespace).Update(ctx, resource, updated, metav1.UpdateOptions{}); err != nil {
		// A conflict proves this conditional write was rejected. Reconcile from fresh
		// metrics/scale instead of replaying an old target against a new resourceVersion.
		if k8serrors.IsConflict(err) || k8serrors.IsNotFound(err) || k8serrors.IsInvalid(err) || k8serrors.IsForbidden(err) {
			trace.write = "rejected"
			if clearErr := r.clearScaleDecision(ctx, instance, decision.ID); clearErr != nil {
				return fmt.Errorf("scale rejected: %v; clear journal: %w", err, clearErr)
			}
		}
		// Transport failures/timeouts have an unknown outcome; leave the durable record.
		return err
	}
	trace.write = "accepted"
	r.Events.ScaleAccepted(instance)
	if err := r.recordScaleDecision(ctx, instance, decision, desired, scale.Status.Replicas); err != nil {
		return err
	}
	return r.clearScaleDecision(ctx, instance, decision.ID)
}

func (r *HPAPlusReconciler) recordScaleDecision(ctx context.Context, instance *api.HPAPlus, decision *api.ScaleDecision, desired, observed int32) error {
	return r.mutateStatus(ctx, instance, func(status *api.HPAPlusStatus) {
		r.projectScaleDecision(instance, status, decision, desired, observed)
	})
}

func (r *HPAPlusReconciler) checkDecisionStatusBudget(instance *api.HPAPlus, decision *api.ScaleDecision, desired, observed int32) error {
	proposed := instance.Status.DeepCopy()
	r.projectScaleDecision(instance, proposed, decision, desired, observed)
	err := checkAccountingBudget(&instance.Status, proposed)
	if err != nil {
		r.Telemetry.ObserveStatusRejection("accounting")
	}
	return err
}

func (r *HPAPlusReconciler) projectScaleDecision(instance *api.HPAPlus, status *api.HPAPlusStatus, decision *api.ScaleDecision, desired, observed int32) {
	behavior := fillBehaviorDefaults(instance.Spec.Behavior)

	if status.LastScaleDecisionID == decision.ID {
		return
	}
	status.LastScaleDecisionID = decision.ID
	status.LastScaleTime = decision.Time.DeepCopy()
	status.CurrentReplicas, status.DesiredReplicas = observed, desired
	status.ScaleUpReplicaHistory, status.ScaleDownReplicaHistory = decision.ScaleUpRecommendations, decision.ScaleDownRecommendations
	event := api.TimestampedSample{Time: decision.Time.DeepCopy()}
	if decision.Desired > decision.PreviousDesired {
		event.Replicas = decision.Desired - decision.PreviousDesired
		status.ScaleUpEventHistory = append(status.ScaleUpEventHistory, event)
	} else {
		event.Replicas = decision.PreviousDesired - decision.Desired
		status.ScaleDownEventHistory = append(status.ScaleDownEventHistory, event)
	}
	status.ScaleUpEventHistory = scalebehavior.PruneTimestampedReplicasToWindow(status.ScaleUpEventHistory, scalebehavior.GetLongestPolicyPeriod(behavior.ScaleUp), r.now())
	status.ScaleDownEventHistory = scalebehavior.PruneTimestampedReplicasToWindow(status.ScaleDownEventHistory, scalebehavior.GetLongestPolicyPeriod(behavior.ScaleDown), r.now())
}

func (r *HPAPlusReconciler) recoverScaleDecision(ctx context.Context, instance *api.HPAPlus, decision *api.ScaleDecision) error {
	if err := history.ValidateDecisionAt(decision, r.now()); err != nil {
		return fmt.Errorf("invalid pending scale decision; preserve journal for repair: %w", err)
	}
	if decision.OwnerUID != string(instance.UID) || decision.Target != instance.Spec.ScaleTargetRef || instance.Status.LastScaleDecisionID == decision.ID {
		return r.clearScaleDecision(ctx, instance, decision.ID)
	}
	scale, _, err := r.getScaleTarget(ctx, instance)
	if k8serrors.IsNotFound(err) {
		return r.clearScaleDecision(ctx, instance, decision.ID)
	}
	if err != nil {
		return err
	}
	if string(scale.UID) != decision.TargetUID || scale.ResourceVersion == decision.ResourceVersion {
		// Recreated target, or no write occurred since the decision was prepared.
		return r.clearScaleDecision(ctx, instance, decision.ID)
	}
	// Kubernetes cannot atomically commit Scale and HPAPlus status. A changed
	// resourceVersion cannot distinguish our write from another actor's write.
	// Conservatively charge this attempted change once, without replaying it.
	log.FromContext(ctx).Info("Recovering scale accounting conservatively", "recoveredDecisionID", observability.Field(decision.ID), "desiredReplicas", scale.Spec.Replicas)
	if err := r.recordScaleDecision(ctx, instance, decision, scale.Spec.Replicas, scale.Status.Replicas); err != nil {
		return err
	}
	r.Events.ScaleRecovered(instance)
	return r.clearScaleDecision(ctx, instance, decision.ID)
}

func validatePendingScaleDecision(decision *api.ScaleDecision) error {
	return history.ValidateDecision(decision)
}

// observeScaleDecision publishes observed replica counts only. Simulated plans
// never alter durable intent, event budgets, cooldown or stabilization history.
func (r *HPAPlusReconciler) observeScaleDecision(ctx context.Context, instance *api.HPAPlus, scale *autoscalingv1.Scale, validity decisionValidity) error {
	return r.mutateStatusChecked(ctx, instance, func(status *api.HPAPlusStatus) error {
		if err := validity.check(r.now()); err != nil {
			return err
		}
		status.CurrentReplicas = scale.Status.Replicas
		status.DesiredReplicas = scale.Spec.Replicas
		return nil
	})
}
