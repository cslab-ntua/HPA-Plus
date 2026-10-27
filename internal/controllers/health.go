package controllers

import (
	"context"
	"encoding/json"
	"errors"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	metricadapter "github.com/cslab-ntua/HPA-Plus/internal/metrics"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type reconcileHealth struct {
	decision             *api.DecisionStatus
	phase                string
	configurationValid   bool
	metricsAvailable     *bool
	predictionsAvailable *bool
	models               []api.ModelStatus
	metrics              *metricadapter.StatusSnapshot
	historyCapacity      *api.HistoryCapacityStatus
}

// publishHealth only writes bounded controller-owned condition values. Detailed
// backend/hook errors stay out of persisted status, where they may expose data.
func (r *HPAPlusReconciler) publishHealth(ctx context.Context, instance *api.HPAPlus, health reconcileHealth, reconcileErr error) error {
	return r.mutateStatus(ctx, instance, func(status *api.HPAPlusStatus) {
		sameGeneration := status.ObservedGeneration == instance.Generation
		status.ObservedGeneration = instance.Generation
		if health.decision != nil {
			status.LastDecision = health.decision.DeepCopy()
		} else if !sameGeneration {
			status.LastDecision = nil
		}
		if health.metrics != nil {
			status.CurrentMetrics = health.metrics.Metrics
			status.CurrentMetricsOmitted = health.metrics.Omitted
		} else if !(sameGeneration && health.phase == "Cooldown") {
			status.CurrentMetrics = nil
			status.CurrentMetricsOmitted = 0
		}
		if health.models != nil {
			status.ModelStatuses, status.ModelStatusesOmitted = boundedModelStatuses(health.models)
		} else if !(sameGeneration && health.phase == "Cooldown") {
			status.ModelStatuses = nil
			status.ModelStatusesOmitted = 0
		}
		set := func(kind string, value metav1.ConditionStatus, reason, message string) {
			meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: kind, Status: value, Reason: reason, Message: message,
				ObservedGeneration: instance.Generation, LastTransitionTime: metav1.Time{Time: r.now()}})
		}
		status.HistoryCapacity = health.historyCapacity.DeepCopy()
		if capacity := health.historyCapacity; capacity == nil {
			set("HistoryCapacityAvailable", metav1.ConditionUnknown, "NotEvaluated", "History capacity could not be assessed in this reconciliation.")
		} else if capacity.CurrentStateBytes > capacity.StateBudgetBytes || capacity.CurrentJournalBytes > capacity.JournalBudgetBytes {
			set("HistoryCapacityAvailable", metav1.ConditionFalse, "CurrentCapacityExceeded", "Stored state exceeds a partition budget; reduce history or stabilization requirements. Existing write limits still apply.")
		} else if capacity.ProjectedStateBytes > capacity.StateBudgetBytes {
			set("HistoryCapacityAvailable", metav1.ConditionFalse, "ProjectedCapacityExceeded", "Full configured histories may exceed the state budget; reduce retained history or model count. This estimate does not disable sampling or scaling.")
		} else if capacity.ProjectedJournalBytes > capacity.JournalBudgetBytes {
			set("HistoryCapacityAvailable", metav1.ConditionFalse, "ProjectedJournalCapacityExceeded", "A future scale journal may exceed its reserved budget at nominal cadence; review stabilization windows and syncPeriod. This estimate does not disable sampling or scaling.")
		} else {
			set("HistoryCapacityAvailable", metav1.ConditionTrue, "WithinBudget", "The estimated full histories and current journal fit their budgets. This assessment does not reserve space.")
		}
		if status.CurrentMetricsOmitted > 0 {
			set("MetricStatusComplete", metav1.ConditionFalse, "MetricReportsOmitted", "Some gathered metrics could not be included in the bounded detailed report.")
		} else if health.metrics != nil {
			set("MetricStatusComplete", metav1.ConditionTrue, "AllGatheredMetricsReported", "Detailed status includes every gathered metric.")
		} else if !(sameGeneration && health.phase == "Cooldown") {
			set("MetricStatusComplete", metav1.ConditionUnknown, "NotEvaluated", "Detailed metric status was not evaluated in this reconciliation.")
		}
		if status.ModelStatusesOmitted > 0 {
			set("ModelStatusComplete", metav1.ConditionFalse, "StatusBudgetExceeded", "Some detailed model reports were omitted to keep status bounded.")
		} else if health.models != nil {
			set("ModelStatusComplete", metav1.ConditionTrue, "AllModelsReported", "Detailed status includes every configured model.")
		} else if !(sameGeneration && health.phase == "Cooldown") {
			set("ModelStatusComplete", metav1.ConditionUnknown, "NotEvaluated", "Detailed model status was not evaluated in this reconciliation.")
		}
		if health.configurationValid {
			set("ConfigurationValid", metav1.ConditionTrue, "ValidConfiguration", "The specification passed controller validation.")
		} else {
			set("ConfigurationValid", metav1.ConditionFalse, "InvalidConfiguration", "The specification failed controller validation; inspect operator logs for details.")
		}
		if health.metricsAvailable != nil {
			if *health.metricsAvailable {
				set("MetricsAvailable", metav1.ConditionTrue, "MetricsReady", "All configured metrics were gathered and evaluated.")
			} else if reconcileErr != nil && health.phase == "MetricEvaluation" {
				set("MetricsAvailable", metav1.ConditionFalse, "MetricEvaluationFailed", "Metrics could not be evaluated safely.")
			} else {
				set("MetricsAvailable", metav1.ConditionFalse, "PartialMetrics", "Some configured metrics are unavailable; downscaling is restricted.")
			}
		} else if !(sameGeneration && health.phase == "Cooldown") {
			set("MetricsAvailable", metav1.ConditionUnknown, "NotEvaluated", "Metrics were not evaluated in this reconciliation.")
		}
		baselineMessage := "Scaling uses the reactive baseline."
		if instance.Spec.PredictionFailurePolicy == api.PredictionFailureHoldCurrent {
			baselineMessage = "Scaling holds the current requested replicas when no forecast is usable."
		}
		for _, model := range instance.Spec.Models {
			if model.RequestDemand != nil {
				baselineMessage = "Scaling uses a valid CPU forecast or holds current requested replicas when request demand is unavailable."
			}
		}
		if instance.Spec.EffectiveMode() == api.ModeObserve {
			baselineMessage = "The reactive baseline is reported without writing Scale."
		}
		if len(instance.Spec.Models) == 0 {
			set("PredictionsAvailable", metav1.ConditionUnknown, "NoModelsConfigured", baselineMessage)
		} else if health.predictionsAvailable != nil {
			if *health.predictionsAvailable {
				set("PredictionsAvailable", metav1.ConditionTrue, "PredictionReady", "At least one usable model prediction is available.")
			} else {
				set("PredictionsAvailable", metav1.ConditionFalse, "NoUsablePrediction", "No usable model prediction is available. "+baselineMessage)
			}
		} else if !(sameGeneration && health.phase == "Cooldown") {
			set("PredictionsAvailable", metav1.ConditionUnknown, "NotEvaluated", "Models were not evaluated in this reconciliation.")
		}
		if !health.configurationValid {
			set("ScalingActive", metav1.ConditionFalse, "InvalidConfiguration", "Scaling is disabled until the specification is corrected.")
		} else if errors.Is(reconcileErr, errStatusCapacity) {
			set("ScalingActive", metav1.ConditionFalse, "StatusCapacityExceeded", "Scaling accounting would exceed the status budget; preserve history and reduce retention.")
		} else if reconcileErr != nil {
			set("ScalingActive", metav1.ConditionFalse, health.phase+"Failed", "The reconciliation could not complete; inspect operator logs for details.")
		} else if instance.Spec.EffectiveMode() == api.ModeObserve {
			set("ScalingActive", metav1.ConditionFalse, "ObserveOnly", "Predictions and counterfactuals are reported; this controller does not write Scale in observe mode.")
		} else {
			set("ScalingActive", metav1.ConditionTrue, health.phase, "The controller is managing the scale target.")
		}
	})
}

func boundedModelStatuses(models []api.ModelStatus) ([]api.ModelStatus, int32) {
	result := make([]api.ModelStatus, 0)
	bytes := 2 // JSON array delimiters.
	for _, model := range models {
		encoded, err := json.Marshal(model)
		if err != nil || len(result) >= 64 || bytes+len(encoded)+1 > 16*1024 {
			continue
		}
		result = append(result, model)
		bytes += len(encoded) + 1
	}
	return result, int32(len(models) - len(result))
}
