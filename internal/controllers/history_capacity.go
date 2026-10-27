package controllers

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/history"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Older supported API servers validate JSON numbers through float64. Keep the
// advisory ceiling exactly representable so an extreme spec cannot break status.
const maxProjectedHistoryBytes int64 = (1 << 53) - 1

// planHistoryCapacity requires a validated specification. It uses arithmetic
// for future rows: even an enormous configured window costs O(models), not
// O(requested samples). Existing payloads are already bounded by the store.
// The estimate is advisory; only actual write sizes enforce storage limits.
func planHistoryCapacity(instance *api.HPAPlus, cm *corev1.ConfigMap, data *api.HPAPlusData) (*api.HistoryCapacityStatus, error) {
	snapshot, err := configMapSnapshot(cm, data)
	if err != nil {
		return nil, err
	}
	return planSnapshotCapacity(instance, snapshot)
}

func planSnapshotCapacity(instance *api.HPAPlus, snapshot *history.Snapshot) (*api.HistoryCapacityStatus, error) {
	unrelated, state, journal := snapshot.UnrelatedBytes, snapshot.StateBytes, snapshot.JournalBytes
	// Workload identity and cache fingerprints are SHA-256 hex strings. Include
	// all lifecycle metadata, even if not used yet, so a fit or failure has room.
	hash := strings.Repeat("f", 64)
	stamp := metav1.NewTime(time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC))
	base := &api.HPAPlusData{TargetIdentity: hash, ModelHistories: map[string]api.ModelHistory{}}
	framing, err := encodeHistory(base)
	if err != nil {
		return nil, err
	}
	projected := int64(len(framing) + unrelated)
	// Reserve signed integer widths and 32 bytes for any finite JSON float.
	cpu, target, metric := int64(math.MinInt64), int32(math.MinInt32), float64(0)
	row := api.TimestampedSample{Time: &stamp, Replicas: math.MinInt32, Metric: &metric,
		TotalCPUUsageMillicores: &cpu, RequestPerPodMillicores: &cpu, TargetCPUUtilizationPercentage: &target}
	rowJSON, err := json.Marshal(row)
	if err != nil {
		return nil, err
	}
	rowBytes := int64(len(rowJSON) + 31) // Replace the single digit 0 with the float allowance.
	for index, model := range instance.Spec.Models {
		metadata := api.ModelHistory{LastObservationID: hash, LastObservationTime: "9999-12-31T23:59:59.999999999Z", Type: model.Type, HistoryEpoch: math.MaxInt64, SyncPeriodsPassed: math.MaxInt,
			StartTime: &stamp, LastSampleTime: &stamp, LastModelRunTime: &stamp, ReplicaHistory: []api.TimestampedSample{},
			FailureState: &api.ModelFailureState{Fingerprint: hash, ConsecutiveFailures: math.MaxInt32, FailedAt: stamp, RetryAfter: stamp},
			CachedPrediction: &api.CachedPrediction{Replicas: math.MaxInt32, Fingerprint: hash, ConsumedUntil: stamp,
				CompletedAt: stamp, ExpiresAt: stamp, HistoryStart: &stamp}}
		if model.PredictivePolicy != nil {
			// Maximum 32 fresh points plus one inherited current interval.
			metadata.CachedPrediction.Trajectory = make([]api.CPUForecastPoint, 33)
			for i := range metadata.CachedPrediction.Trajectory {
				metadata.CachedPrediction.Trajectory[i] = api.CPUForecastPoint{At: "9999-12-31T23:59:59.999999999Z", Until: "9999-12-31T23:59:59.999999999Z", CPU: math.MaxInt64}
			}
		}
		if model.ForecastReliability != nil {
			// Bound 128 pending endpoints plus 100 calibration and 20 recent
			// errors, strings and framing. Actual store budgets remain binding.
			projected = saturatingCapacityAdd(projected, 64*1024)
		}
		entry, err := json.Marshal(metadata)
		if err != nil {
			return nil, err
		}
		name, err := json.Marshal(model.Name)
		if err != nil {
			return nil, err
		}
		projected = saturatingCapacityAdd(projected, int64(len(name)+1+len(entry)))
		if index > 0 {
			projected = saturatingCapacityAdd(projected, 1) // Map entry comma.
		}
		count := int64(requiredHistorySize(&model))
		if count > 0 {
			// [] is already in metadata; N rows need N-1 separating commas.
			rows := maxProjectedHistoryBytes
			if count <= maxProjectedHistoryBytes/(rowBytes+1) {
				rows = count*(rowBytes+1) - 1
			}
			projected = saturatingCapacityAdd(projected, rows)
		}
	}
	journalPlan, err := projectedJournalCapacity(instance)
	if err != nil {
		return nil, err
	}
	if int64(journal) > journalPlan {
		journalPlan = int64(journal)
	}
	return &api.HistoryCapacityStatus{ProjectedStateBytes: projected, ProjectedJournalBytes: journalPlan, CurrentStateBytes: int64(state),
		CurrentJournalBytes: int64(journal), StateBudgetBytes: int64(snapshot.StateBudgetBytes), JournalBudgetBytes: int64(snapshot.JournalBudgetBytes)}, nil
}

// This is a nominal forecast, not a hard bound: watch-triggered reconciliations
// can add recommendations faster than syncPeriod. Actual write budgets remain
// authoritative. No future recommendation arrays are allocated.
func projectedJournalCapacity(instance *api.HPAPlus) (int64, error) {
	stamp := metav1.NewTime(time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC))
	identity := strings.Repeat("f", 64)
	owner := string(instance.UID)
	if len(owner) < len(identity) {
		owner = identity
	}
	journal := &api.ScaleDecision{ID: identity, OwnerUID: owner, Target: instance.Spec.ScaleTargetRef, TargetUID: identity,
		ResourceVersion: identity, PreviousDesired: math.MaxInt32, Desired: math.MaxInt32, Observed: math.MaxInt32, Time: stamp,
		ScaleUpRecommendations: []api.TimestampedSample{}, ScaleDownRecommendations: []api.TimestampedSample{}}
	raw, err := json.Marshal(journal)
	if err != nil {
		return 0, err
	}
	// Count the envelope comma and pendingScale field framing, as the write budget does.
	projected := int64(len(raw) + len(`,"pendingScale":`))
	cpu, target, metric := int64(math.MinInt64), int32(math.MinInt32), float64(0)
	row := api.TimestampedSample{Time: &stamp, Replicas: math.MinInt32, Metric: &metric, TotalCPUUsageMillicores: &cpu,
		RequestPerPodMillicores: &cpu, TargetCPUUtilizationPercentage: &target}
	rowJSON, err := json.Marshal(row)
	if err != nil {
		return 0, err
	}
	rowBytes := int64(len(rowJSON) + 31)
	behavior := fillBehaviorDefaults(instance.Spec.Behavior)
	period := syncPeriodForInstance(instance)
	if period <= 0 {
		return 0, fmt.Errorf("capacity projection requires a positive syncPeriod")
	}
	for _, direction := range []struct {
		window   int32
		retained int
	}{
		{*behavior.ScaleUp.StabilizationWindowSeconds, len(instance.Status.ScaleUpReplicaHistory)},
		{*behavior.ScaleDown.StabilizationWindowSeconds, len(instance.Status.ScaleDownReplicaHistory)},
	} {
		window := time.Duration(direction.window) * time.Second
		count := int64(1)
		if window > 0 {
			count += int64(window / period)
			if window%period != 0 {
				count++
			}
		}
		if existing := int64(direction.retained) + 1; existing > count {
			count = existing
		}
		rows := maxProjectedHistoryBytes
		if count <= maxProjectedHistoryBytes/(rowBytes+1) {
			rows = count*(rowBytes+1) - 1
		}
		projected = saturatingCapacityAdd(projected, rows)
	}
	return projected, nil
}

func saturatingCapacityAdd(a, b int64) int64 {
	if b > maxProjectedHistoryBytes-a {
		return maxProjectedHistoryBytes
	}
	return a + b
}
