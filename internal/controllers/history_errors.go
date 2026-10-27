package controllers

import (
	"context"
	"errors"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
)

// Reasons are fixed operator-owned categories. Underlying errors remain useful
// for errors.Is classification, but operational logs, status and metric labels
// never copy their text.
type historyStorageError struct {
	reason string
	err    error
}

func (e *historyStorageError) Error() string { return e.err.Error() }
func (e *historyStorageError) Unwrap() error { return e.err }

func storageFailure(reason string, err error) error {
	if err == nil {
		return nil
	}
	var existing *historyStorageError
	if errors.As(err, &existing) {
		return err
	}
	return &historyStorageError{reason: reason, err: err}
}

func historyOperationOutcome(ctx context.Context, err error, success string) string {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "canceled"
	}
	if err == nil {
		return success
	}
	if k8serrors.IsConflict(err) {
		return "conflict"
	}
	var storage *historyStorageError
	if errors.As(err, &storage) {
		switch storage.reason {
		case "HistoryCapacityExceeded":
			return "capacity"
		case "HistoryDataInvalid":
			return "invalid"
		case "HistoryOwnershipMismatch":
			return "ownership"
		}
	}
	return "failed"
}
