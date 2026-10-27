package history

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
)

// ErrFutureWindow rejects otherwise structurally valid history without treating
// a possible wall-clock regression as permanent data corruption. Callers must
// not forecast from it or automatically reset it; retry after time catches up.
var ErrFutureWindow = errors.New("history timestamps are ahead of the current clock")

// validateStoredHistory checks invariants of samples produced by the sampler.
// CPU-less legacy rows are ignored here and removed during sampler preparation.
// Reject an entire damaged training window instead of silently joining samples
// across gaps or choosing between conflicting observations at the same timestamp.
func ValidateWindow(history api.ModelHistory, now time.Time) error {
	future := false
	if history.LastObservationID != "" || history.LastObservationTime != "" {
		id, err := hex.DecodeString(history.LastObservationID)
		stamp, timeErr := time.Parse(time.RFC3339Nano, history.LastObservationTime)
		if err != nil || len(id) != 32 || timeErr != nil || stamp.IsZero() {
			return fmt.Errorf("invalid source observation marker")
		}
		future = stamp.After(now)
	}
	if history.HistoryEpoch < 0 {
		return fmt.Errorf("history epoch is negative")
	}
	if history.SyncPeriodsPassed < 0 {
		return fmt.Errorf("sample progress is negative")
	}
	if !prediction.UsesCPUHistory(history.Type) {
		if future {
			return ErrFutureWindow
		}
		return nil
	}
	var previous *time.Time
	for i, sample := range history.ReplicaHistory {
		if sample.TotalCPUUsageMillicores == nil {
			continue
		}
		if sample.Time == nil || sample.Time.IsZero() {
			return fmt.Errorf("sample %d has a missing or zero timestamp", i)
		}
		future = future || sample.Time.After(now)
		if previous != nil && !sample.Time.After(*previous) {
			return fmt.Errorf("sample %d is not strictly chronological", i)
		}
		if *sample.TotalCPUUsageMillicores < 0 {
			return fmt.Errorf("sample %d has negative CPU usage", i)
		}
		if sample.Replicas < 0 || (sample.RequestPerPodMillicores != nil && *sample.RequestPerPodMillicores <= 0) ||
			(sample.TargetCPUUtilizationPercentage != nil && *sample.TargetCPUUtilizationPercentage <= 0) {
			return fmt.Errorf("sample %d has invalid replica or capacity metadata", i)
		}
		if sample.Metric != nil && (math.IsNaN(*sample.Metric) || math.IsInf(*sample.Metric, 0) || *sample.Metric < 0) {
			return fmt.Errorf("sample %d has invalid CPU utilization", i)
		}
		previous = &sample.Time.Time
	}
	if history.LastSampleTime != nil {
		if history.LastSampleTime.IsZero() {
			return fmt.Errorf("last sample timestamp is zero")
		}
		future = future || history.LastSampleTime.After(now)
	}
	if history.LastModelRunTime != nil && (history.LastModelRunTime.IsZero() ||
		previous == nil || history.LastModelRunTime.After(*previous)) {
		return fmt.Errorf("consumed timestamp is outside retained history")
	}
	if future {
		return ErrFutureWindow
	}
	return nil
}

func ResetWindow(history *api.ModelHistory) error {
	if history.HistoryEpoch < 0 {
		return fmt.Errorf("invalid history epoch; preserve state for repair")
	}
	if history.HistoryEpoch == math.MaxInt64 {
		return fmt.Errorf("history epoch exhausted")
	}
	history.ReplicaHistory = nil
	history.HistoryEpoch++
	history.SyncPeriodsPassed = 0
	history.LastModelRunTime = nil
	history.LastSampleTime = nil
	history.LastObservationID = ""
	history.LastObservationTime = ""
	history.Reliability = nil
	history.CachedPrediction = nil
	history.FailureState = nil
	return nil
}
