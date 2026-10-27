package controllers

import (
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"reflect"
)

// Only forecast-owned fields fence a completion. Sampler appends and unrelated
// journal/model writes are deliberately absent. Re-evaluate after every CAS retry.
func sameCompletionState(current api.ModelHistory, expected []*api.ModelHistory) bool {
	if len(expected) == 0 {
		return true
	} // compatibility for low-level callers
	if len(expected) != 1 || expected[0] == nil {
		return false
	}
	before := expected[0]
	return reflect.DeepEqual(current.Reliability, before.Reliability) &&
		reflect.DeepEqual(current.LastModelRunTime, before.LastModelRunTime) &&
		reflect.DeepEqual(current.CachedPrediction, before.CachedPrediction) &&
		reflect.DeepEqual(current.FailureState, before.FailureState)
}
