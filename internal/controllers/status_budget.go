package controllers

import (
	"encoding/json"
	"errors"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

const maxStatusBytes = 256 * 1024

// Leave room for bounded metric/model reports, conditions and timing metadata.
const maxAccountingStatusBytes = maxStatusBytes - 64*1024

var errStatusCapacity = errors.New("status capacity exceeded; preserve scaling history and reduce configured retention")

func statusBytes(status *api.HPAPlusStatus) (int, error) {
	raw, err := json.Marshal(status)
	return len(raw), err
}

// Existing oversized status may shrink or remain the same size, allowing repair
// and journal recovery. No safety history or foreign condition is silently dropped.
func checkStatusBudget(base, proposed *api.HPAPlusStatus, limit int) error {
	size, err := statusBytes(proposed)
	if err != nil {
		return err
	}
	if size <= limit {
		return nil
	}
	previous, err := statusBytes(base)
	if err != nil {
		return err
	}
	if previous > limit && size <= previous {
		return nil
	}
	return errStatusCapacity
}

func accountingStatus(status *api.HPAPlusStatus) *api.HPAPlusStatus {
	result := status.DeepCopy()
	result.ModelStatuses, result.CurrentMetrics = nil, nil
	result.ModelStatusesOmitted, result.CurrentMetricsOmitted = 0, 0
	return result
}

func checkAccountingBudget(base, proposed *api.HPAPlusStatus) error {
	return checkStatusBudget(accountingStatus(base), accountingStatus(proposed), maxAccountingStatusBytes)
}
