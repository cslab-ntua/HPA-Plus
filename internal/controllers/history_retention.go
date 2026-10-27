package controllers

import (
	"context"
	"fmt"
	"math"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

// historyInputDeadline is exclusive, matching prediction validity checks. Input
// timestamps are validated separately; CPU-less legacy rows are not training data.
func historyInputDeadline(model *api.Model, rows []api.TimestampedSample) *time.Time {
	if model.HistoryMaxAge == nil {
		return nil
	}
	var first *time.Time
	for _, row := range rows {
		if row.TotalCPUUsageMillicores != nil && row.Time != nil && (first == nil || row.Time.Time.Before(*first)) {
			stamp := row.Time.Time
			first = &stamp
		}
	}
	if first == nil {
		return nil
	}
	expires := first.Add(time.Duration(*model.HistoryMaxAge) * time.Millisecond)
	return &expires
}

func pruneAgedHistory(model *api.Model, history *api.ModelHistory, now time.Time) (bool, error) {
	if model.HistoryMaxAge == nil {
		return false, nil
	}
	cutoff := now.Add(-time.Duration(*model.HistoryMaxAge) * time.Millisecond)
	kept := make([]api.TimestampedSample, 0, len(history.ReplicaHistory))
	for _, row := range history.ReplicaHistory {
		if row.Time != nil && row.Time.After(cutoff) {
			kept = append(kept, row)
		}
	}
	if len(kept) == len(history.ReplicaHistory) {
		return false, nil
	}
	if history.HistoryEpoch == math.MaxInt64 {
		return false, fmt.Errorf("history epoch exhausted")
	}
	history.ReplicaHistory = kept
	history.HistoryEpoch++
	history.CachedPrediction, history.FailureState = nil, nil
	history.Reliability = nil
	history.LastModelRunTime = nil
	history.SyncPeriodsPassed = 0
	// Preserve start and last-sampled slots: expiration is not a new scheduled
	// start and must not allow duplicate observations for the current slot.
	return true, nil
}

// Reconciliation enforces retention too, independently of sampler timing. The
// mutation rereads current history so concurrent samples and journals survive.
func (r *HPAPlusReconciler) retainModelHistory(ctx context.Context, instance *api.HPAPlus, model *api.Model, target string) (api.ModelHistory, error) {
	var retained api.ModelHistory
	err := r.mutateConfigMapData(ctx, instance, func(data *api.HPAPlusData) (bool, error) {
		if data.TargetIdentity != target {
			return false, fmt.Errorf("scale target changed while retaining history")
		}
		history, exists := data.ModelHistories[model.Name]
		retained = history
		if !exists || history.Type != model.Type || validateStoredHistory(history, r.now()) != nil {
			return false, nil // Existing readiness/corruption handling owns these cases.
		}
		changed, err := pruneAgedHistory(model, &history, r.now())
		if err != nil {
			return false, err
		}
		if changed {
			data.ModelHistories[model.Name] = history
		}
		retained = history
		return changed, nil
	})
	return retained, err
}

func hasExpiredTraining(model *api.Model, history api.ModelHistory, now time.Time) bool {
	deadline := historyInputDeadline(model, history.ReplicaHistory)
	return deadline != nil && !now.Before(*deadline)
}
