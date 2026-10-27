package controllers

import (
	"context"
	"fmt"
	"math"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const maximumModelBackoff = 5 * time.Minute

// RetryPolicy opens the circuit after each failed fit. A due reconciliation is
// its next probe; successful fits clear persisted state. Configure before use.
// Zero Initial follows syncPeriod; zero Maximum selects the historical five minutes.
type RetryPolicy struct{ Initial, Maximum time.Duration }

const maximumRetryPolicyDelay = 24 * time.Hour

func (p RetryPolicy) Validate() error {
	maximum := p.maximum()
	if p.Initial < 0 || (p.Initial > 0 && p.Initial < time.Second) || maximum < time.Second || maximum > maximumRetryPolicyDelay || p.Initial > maximum {
		return fmt.Errorf("retry policy requires initial=0 or >=1s and initial <= maximum <=24h")
	}
	return nil
}
func (p RetryPolicy) maximum() time.Duration {
	if p.Maximum == 0 {
		return maximumModelBackoff
	}
	return p.Maximum
}
func modelRetryDelay(instance *api.HPAPlus, failures int32) time.Duration {
	return (RetryPolicy{}).delay(instance, failures)
}
func (p RetryPolicy) delay(instance *api.HPAPlus, failures int32) time.Duration {
	// Reject malformed programmatic configuration by falling back to safe defaults.
	if p.Validate() != nil {
		p = RetryPolicy{}
	}
	maximum := p.maximum()
	delay := p.Initial
	if delay == 0 {
		delay = syncPeriodForInstance(instance)
	}
	if delay < time.Second {
		delay = time.Second
	}
	if delay > maximum {
		delay = maximum
	}
	for i := int32(1); i < failures && delay < maximum; i++ {
		if delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	return delay
}

func validFailureState(state *api.ModelFailureState, fingerprint string, now time.Time) bool {
	return (RetryPolicy{}).validFailureState(state, fingerprint, now)
}
func (p RetryPolicy) validFailureState(state *api.ModelFailureState, fingerprint string, now time.Time) bool {
	return state != nil && fingerprint != "" && state.Fingerprint == fingerprint && state.ConsecutiveFailures > 0 &&
		!state.FailedAt.IsZero() && !state.FailedAt.After(now) && state.RetryAfter.After(state.FailedAt.Time) &&
		!state.RetryAfter.After(state.FailedAt.Add(p.maximum()+time.Second))
}

func modelInBackoff(history api.ModelHistory, fingerprint string, now time.Time) bool {
	return (RetryPolicy{}).inBackoff(history, fingerprint, now)
}
func (p RetryPolicy) inBackoff(history api.ModelHistory, fingerprint string, now time.Time) bool {
	return p.validFailureState(history.FailureState, fingerprint, now) && now.Before(history.FailureState.RetryAfter.Time)
}

func (r *HPAPlusReconciler) persistModelFailure(ctx context.Context, instance *api.HPAPlus, name string, epoch int64, targetIdentity, fingerprint string, trainingDeadline *time.Time, expected ...*api.ModelHistory) error {
	// Leader shutdown/cancellation is not a model failure. A model's own deadline
	// is separate from this parent context and still counts as an unsuccessful fit.
	if err := ctx.Err(); err != nil {
		return err
	}
	now := r.now()
	return r.mutateConfigMapData(ctx, instance, func(data *api.HPAPlusData) (bool, error) {
		// A conflict can outlive the input window even when the fit failed while
		// its samples were still current. Do not back off that expired window.
		if trainingDeadline != nil && !r.now().Before(*trainingDeadline) {
			return false, errPredictionsExpired
		}
		history, exists := data.ModelHistories[name]
		if !exists || data.TargetIdentity != targetIdentity || history.HistoryEpoch != epoch || validateStoredHistory(history, now) != nil {
			return false, nil
		}
		if !sameCompletionState(history, expected) {
			return false, nil
		}
		failures := int32(1)
		if r.RetryPolicy.validFailureState(history.FailureState, fingerprint, now) {
			failures = history.FailureState.ConsecutiveFailures
			if failures < math.MaxInt32 {
				failures++
			}
		}
		retry := now.Add(r.RetryPolicy.delay(instance, failures))
		// ConfigMap metav1.Time fields persist seconds. Round the retry up so
		// serialization cannot shorten the delay after an operator restart.
		if retry.Nanosecond() != 0 {
			retry = retry.Truncate(time.Second).Add(time.Second)
		}
		history.FailureState = &api.ModelFailureState{Fingerprint: fingerprint, ConsecutiveFailures: failures,
			FailedAt: metav1.Time{Time: now.UTC().Truncate(time.Second)}, RetryAfter: metav1.Time{Time: retry.UTC()}}
		data.ModelHistories[name] = history
		return true, nil
	})
}
