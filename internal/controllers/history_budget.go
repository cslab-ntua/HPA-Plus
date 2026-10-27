package controllers

import (
	"fmt"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

const (
	// Keep room for a write-ahead scale decision before a growing training
	// window exhausts the shared ConfigMap. Never reclaim space by silently
	// discarding samples: configured window sizes also define model readiness.
	maxHistoryStateBytes = 768 << 10
	maxScaleJournalBytes = maxConfigMapDataBytes - maxHistoryStateBytes
)

// historyBudgetParts includes the envelope, model state and unrelated keys in
// the state budget; the journal budget includes its JSON field framing. Their
// sum is the exact proposed combined ConfigMap data size.
func historyBudgetParts(data *api.HPAPlusData, payloadBytes, unrelatedBytes int) (state, journal int, err error) {
	if data.PendingScale == nil {
		return payloadBytes + unrelatedBytes, 0, nil
	}
	withoutJournal := *data
	withoutJournal.PendingScale = nil
	encoded, err := encodeHistory(&withoutJournal)
	if err != nil {
		return 0, 0, err
	}
	return len(encoded) + unrelatedBytes, payloadBytes - len(encoded), nil
}

func checkHistoryBudget(previous *corev1.ConfigMap, data *api.HPAPlusData, payloadBytes int) error {
	unrelated := configMapDataBytes(previous) - len(previous.Data[configMapDataKey])
	state, journal, err := historyBudgetParts(data, payloadBytes, unrelated)
	if err != nil {
		return storageFailure("HistoryDataInvalid", err)
	}
	if state <= maxHistoryStateBytes && journal <= maxScaleJournalBytes {
		return nil
	}
	// An upgrade must not strand recovery or prevent removal of old models.
	// Grandfather each oversized partition independently, only without growth.
	// Decode and normalize old data so whitespace or legacy framing cannot buy
	// extra capacity. The absolute 1 MiB limit still applies to every write.
	oldState, oldJournal := 0, 0
	if old, decodeErr := decodeHistory(previous.Data[configMapDataKey]); decodeErr == nil {
		if encoded, encodeErr := encodeHistory(old); encodeErr == nil {
			oldState, oldJournal, _ = historyBudgetParts(old, len(encoded), unrelated)
		}
	}
	if state > maxHistoryStateBytes && state > oldState {
		return storageFailure("HistoryCapacityExceeded", fmt.Errorf("history state and unrelated ConfigMap data require %d bytes; budget is %d; reduce retained history or model count", state, maxHistoryStateBytes))
	}
	if journal > maxScaleJournalBytes && journal > oldJournal {
		return storageFailure("HistoryCapacityExceeded", fmt.Errorf("pending scale journal requires %d bytes; budget is %d; reduce stabilization history", journal, maxScaleJournalBytes))
	}
	return nil
}
