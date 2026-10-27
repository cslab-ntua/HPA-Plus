package prediction

import (
	"sort"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

// CloneHistory gives sorting and pruning their own slice. Sample pointer fields
// remain shared and read-only; callers that edit sample values must deep-copy.
func CloneHistory(history []api.TimestampedSample) []api.TimestampedSample {
	if history == nil {
		return nil
	}
	result := make([]api.TimestampedSample, len(history))
	copy(result, history)
	return result
}

// FilterCPUHistory preserves input order, including the legacy ARIMA one-shot
// path with missing timestamps. CPU zero is a valid observation.
func FilterCPUHistory(history []api.TimestampedSample) []api.TimestampedSample {
	result := make([]api.TimestampedSample, 0, len(history))
	for _, entry := range history {
		if entry.TotalCPUUsageMillicores != nil {
			result = append(result, entry)
		}
	}
	return result
}

func CountCPUHistory(history []api.TimestampedSample) int {
	count := 0
	for _, entry := range history {
		if entry.TotalCPUUsageMillicores != nil {
			count++
		}
	}
	return count
}

// PruneCPUHistory retains the newest CPU-backed window and any interleaved
// legacy rows inside it. Controller sampling separately removes CPU-less rows.
// A window already within its CPU limit keeps its existing order. Pruning uses
// chronological order, treats undated rows as oldest, and preserves ties.
func PruneCPUHistory(history []api.TimestampedSample, limit int) []api.TimestampedSample {
	if limit <= 0 {
		return []api.TimestampedSample{}
	}
	result := CloneHistory(history)
	if CountCPUHistory(result) <= limit {
		return result
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Time == nil {
			return result[j].Time != nil
		}
		if result[j].Time == nil {
			return false
		}
		return result[i].Time.Before(result[j].Time)
	})
	count := 0
	for i := len(result) - 1; i >= 0; i-- {
		if result[i].TotalCPUUsageMillicores != nil {
			count++
		}
		if count == limit {
			// Do not retain discarded rows through the backing array.
			return CloneHistory(result[i:])
		}
	}
	return result
}

// MetricHistory returns an aligned auxiliary series only if every row has it.
func MetricHistory(history []api.TimestampedSample) []float64 {
	if len(history) == 0 {
		return nil
	}
	values := make([]float64, 0, len(history))
	for _, entry := range history {
		if entry.Metric == nil {
			return nil
		}
		values = append(values, *entry.Metric)
	}
	return values
}
