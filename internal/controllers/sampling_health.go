package controllers

import (
	"context"
	"errors"
	"fmt"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/history"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type samplingReport struct {
	outcome, phase string
	nextCheck      *time.Time
	sampledSlot    time.Time
}

// Only the sampling runnable owns SamplingActive. Reconciliation updates its
// own conditions through the same optimistic status helper, preserving this one.
func (r *HPAPlusReconciler) publishSamplingHealth(ctx context.Context, instance *api.HPAPlus, report samplingReport, sampleErr error) error {
	return r.mutateStatus(ctx, instance, func(status *api.HPAPlusStatus) {
		previous := meta.FindStatusCondition(status.Conditions, "SamplingActive")
		if sampleErr == nil && report.outcome == "not_due" && previous != nil && previous.ObservedGeneration == instance.Generation {
			return // A scheduling check cannot clear the last actual sampling failure.
		}
		value, reason, message := metav1.ConditionUnknown, "AwaitingSample", "The sampler has not recorded an outcome for this specification."
		switch report.outcome {
		case "completed":
			value, reason, message = metav1.ConditionTrue, "SamplesPersisted", "The last due sampling attempt persisted usable CPU history."
		case "duplicate_source":
			value, reason, message = metav1.ConditionTrue, "AwaitingNewObservation", "The source measurement has already been sampled; training will resume when source time advances."
		case "missing_cpu":
			value, reason, message = metav1.ConditionFalse, "CPUUnavailable", "The last sampling attempt had no usable CPU observation."
		case "invalid":
			value, reason, message = metav1.ConditionFalse, "InvalidConfiguration", "Sampling is disabled until the specification is corrected."
		case "no_models":
			reason, message = "NoModelsConfigured", "No models require training history."
		}
		if sampleErr != nil {
			value, reason, message = metav1.ConditionFalse, "SamplingFailed", "The last sampling attempt failed; inspect operator logs for details."
			var storage *historyStorageError
			if errors.As(sampleErr, &storage) {
				reason = storage.reason
			} else {
				switch report.phase {
				case "TargetRead", "MetricEvaluation", "HistoryPreparation", "HistoryBinding", "HistoryWrite":
					reason = report.phase + "Failed"
				}
			}
			if errors.Is(sampleErr, context.DeadlineExceeded) {
				reason = "SamplingTimedOut"
			}
			if errors.Is(sampleErr, history.ErrFutureWindow) {
				reason = "HistoryClockSkew"
				message = "Stored history is ahead of the current clock; history is preserved and sampling waits for time to catch up. Inspect clock synchronization or repair incorrect timestamps."
			}
		}
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: "SamplingActive", Status: value, Reason: reason, Message: message,
			ObservedGeneration: instance.Generation, LastTransitionTime: metav1.NewTime(r.now())})
	})
}

func (s *HistorySampler) reportSampling(parent context.Context, instance *api.HPAPlus, report samplingReport, sampleErr error) error {
	if parent.Err() != nil {
		return sampleErr
	}
	// The attempt may have exhausted its deadline. Use a separate short budget,
	// still canceled by leader/manager shutdown, to report the timeout itself.
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	if err := s.Controller.publishSamplingHealth(ctx, instance, report, sampleErr); err != nil {
		if parent.Err() == nil {
			s.Controller.Events.StatusFailed(instance)
		}
		if sampleErr != nil {
			return fmt.Errorf("%w; publishing sampling health failed: %v", sampleErr, err)
		}
		return fmt.Errorf("publishing sampling health failed: %w", err)
	}
	return sampleErr
}
