// Package decision plans replica decisions without clients, clocks or writes.
package decision

import (
	"fmt"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/scalebehavior"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Input contains one cycle's already validated specification and observations.
// Requested is the Scale spec count, not the lagging observed pod count.
type Input struct {
	Spec                api.HPAPlusSpec
	Status              api.HPAPlusStatus
	Now                 time.Time
	Requested, Baseline int32
	Predictions         []int32
	// WindowPredictions are the same models' counts without the rules that
	// only keep the current pods for one cycle. The scale-down window stores
	// them, so such a hold delays one decision and not the whole window. Nil,
	// or a length other than Predictions', stores the counts as decided.
	WindowPredictions []int32
	MetricsIncomplete bool
}

// Result is a reviewable plan. Persistence and write-time freshness/identity
// checks remain the controller's responsibility; planning has no side effects.
type Result struct {
	RolloutMode                     string
	BaselineDesired, ControlDesired int32

	Constraints                                      []string
	Mode                                             string
	BaselineIncluded, MetricsHeld                    bool
	Selected, Guarded, Desired                       int32
	ScaleUpRecommendations, ScaleDownRecommendations []api.TimestampedSample
}

// Plan computes independent reactive/control counterfactuals from the same real
// accounting history. Observe never appends hypothetical recommendations to it.
func Plan(in Input) (Result, error) {
	mode := in.Spec.EffectiveMode()
	if mode != api.ModeObserve && mode != api.ModeAssist && mode != api.ModeControl {
		return Result{}, fmt.Errorf("invalid rollout mode")
	}
	control, err := plan(in, false)
	if err != nil {
		return Result{}, err
	}
	baselineInput := in
	baselineInput.Predictions, baselineInput.WindowPredictions = nil, nil
	baselineInput.Spec = *in.Spec.DeepCopy()
	baselineInput.Spec.Models = nil
	baselineInput.Spec.PredictionFailurePolicy = api.PredictionFailureReactive
	// Removing predictions cannot invalidate the already validated input/strategy.
	baseline, _ := plan(baselineInput, false)
	result := control
	if mode == api.ModeAssist {
		result, _ = plan(in, true)
	}
	result.RolloutMode = mode
	result.BaselineDesired, result.ControlDesired = baseline.Desired, control.Desired
	if mode == api.ModeObserve {
		result.Desired = in.Requested
		result.ScaleUpRecommendations, result.ScaleDownRecommendations = nil, nil
	}
	return result, nil
}

func plan(in Input, assist bool) (Result, error) {
	min := int32(defaultMinReplicas)
	if in.Spec.MinReplicas != nil {
		min = *in.Spec.MinReplicas
	}
	if in.Now.IsZero() || in.Requested < 0 || in.Baseline < 0 || min < 0 || in.Spec.MaxReplicas < min {
		return Result{}, fmt.Errorf("invalid decision inputs")
	}
	strategy := defaultDecisionType
	if in.Spec.DecisionType != nil {
		strategy = *in.Spec.DecisionType
	}
	holdFailure := in.Spec.PredictionFailurePolicy == api.PredictionFailureHoldCurrent
	for _, model := range in.Spec.Models {
		if model.RequestDemand != nil {
			holdFailure = true
		}
	}
	result := Result{Mode: "baseline", BaselineIncluded: (len(in.Predictions) == 0 && !holdFailure) || (len(in.Predictions) > 0 && in.Spec.IncludeHPA != nil && *in.Spec.IncludeHPA)}
	candidates := append([]int32(nil), in.Predictions...)
	var window []int32
	if len(in.WindowPredictions) > 0 && len(in.WindowPredictions) == len(in.Predictions) {
		window = append(window, in.WindowPredictions...)
	}
	if len(candidates) == 0 && holdFailure {
		candidates = append(candidates, in.Requested)
		result.Mode = "hold"
		result.Constraints = append(result.Constraints, "prediction_unavailable_hold")
	}
	if len(in.Predictions) > 0 {
		result.Mode = "predictive"
		if result.BaselineIncluded {
			result.Mode = "mixed"
		}
	}
	if result.BaselineIncluded {
		candidates = append(candidates, in.Baseline)
		if window != nil {
			window = append(window, in.Baseline)
		}
	}
	selected, err := scalebehavior.DecideTargetReplicasByScalingStrategy(strategy, candidates)
	if err != nil {
		return Result{}, err
	}
	if assist {
		result.BaselineIncluded = true
		if len(in.Predictions) > 0 {
			result.Mode = "mixed"
		}
		if selected < in.Baseline {
			selected = in.Baseline
			result.Constraints = append(result.Constraints, "assist_baseline")
		}
	}
	result.Selected, result.Guarded = selected, selected
	if in.MetricsIncomplete && selected < in.Requested {
		result.MetricsHeld = true
		result.Guarded = in.Requested
	}
	// The scale-down window keeps what was asked for, not a count held for this
	// cycle only (partial metrics, or a model rule that keeps the current pods).
	// This cycle still decides on the held count.
	recommended := selected
	if window != nil {
		if value, err := scalebehavior.DecideTargetReplicasByScalingStrategy(strategy, window); err == nil {
			if assist && value < in.Baseline {
				value = in.Baseline
			}
			if value < recommended {
				recommended = value
			}
		}
	}
	behavior := BehaviorDefaults(in.Spec.Behavior)
	status := in.Status.DeepCopy()
	upEvents := scalebehavior.PruneTimestampedReplicasToWindow(status.ScaleUpEventHistory, scalebehavior.GetLongestPolicyPeriod(behavior.ScaleUp), in.Now)
	downEvents := scalebehavior.PruneTimestampedReplicasToWindow(status.ScaleDownEventHistory, scalebehavior.GetLongestPolicyPeriod(behavior.ScaleDown), in.Now)
	row := api.TimestampedSample{Time: &metav1.Time{Time: in.Now}, Replicas: result.Guarded}
	result.ScaleUpRecommendations = append(scalebehavior.PruneTimestampedReplicasToWindow(status.ScaleUpReplicaHistory, *behavior.ScaleUp.StabilizationWindowSeconds, in.Now), row)
	result.ScaleDownRecommendations = append(scalebehavior.PruneTimestampedReplicasToWindow(status.ScaleDownReplicaHistory, *behavior.ScaleDown.StabilizationWindowSeconds, in.Now),
		api.TimestampedSample{Time: &metav1.Time{Time: in.Now}, Replicas: recommended})
	details := scalebehavior.DecideTargetReplicasByBehaviorDetails(behavior, in.Requested, result.Guarded, min, in.Spec.MaxReplicas,
		result.ScaleUpRecommendations, result.ScaleDownRecommendations, upEvents, downEvents, in.Now)
	result.Desired = details.Desired
	if result.MetricsHeld {
		result.Constraints = append(result.Constraints, "partial_metrics")
	}
	if details.Stabilized != result.Guarded {
		result.Constraints = append(result.Constraints, "stabilization")
	}
	if details.RateLimited != details.Stabilized {
		reason := "rate_limit"
		if (details.Stabilized > in.Requested && *behavior.ScaleUp.SelectPolicy == autoscalingv2.DisabledPolicySelect) || (details.Stabilized < in.Requested && *behavior.ScaleDown.SelectPolicy == autoscalingv2.DisabledPolicySelect) {
			reason = "disabled"
		}
		result.Constraints = append(result.Constraints, reason)
	}
	if details.RateLimited < min {
		result.Constraints = append(result.Constraints, "minimum")
	}
	if details.RateLimited > in.Spec.MaxReplicas {
		result.Constraints = append(result.Constraints, "maximum")
	}
	return result, nil
}
