/*
Copyright 2023 The Predictive Horizontal Pod Autoscaler Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package scalebehavior provides functions for managing the scaling behavior of the HPA+.
// This applies HPA scaling behaviors (downscale stabilization, scale rules), HPA+ scaling strategies, and min/max
// replicas.
// Much of the code for this package has been directly copied from the Kubernetes source code here:
// https://github.com/kubernetes/kubernetes/blob/3e26e104bdf9d0dc3c4046d6350b93557c67f3f4/pkg/controller/podautoscaler/horizontal.go
package scalebehavior

import (
	"fmt"
	"math"
	"sort"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

func DecideTargetReplicasByScalingStrategy(decisionType string, predictedReplicas []int32) (int32, error) {
	if len(predictedReplicas) == 0 {
		return 0, fmt.Errorf("no replica candidates")
	}
	for _, value := range predictedReplicas {
		if value < 0 {
			return 0, fmt.Errorf("negative replica candidate")
		}
	}
	predictedReplicas = append([]int32(nil), predictedReplicas...)

	// Sort in ascending order
	sort.Slice(predictedReplicas, func(i, j int) bool { return predictedReplicas[i] < predictedReplicas[j] })

	// Decide which replica count to use based on decision type
	var targetReplicas int32
	switch decisionType {
	case hpaplusv1alpha1.DecisionMaximum:
		max := int32(0)
		for i, predictedReplica := range predictedReplicas {
			if i == 0 || predictedReplica > max {
				max = predictedReplica
			}
		}
		targetReplicas = max
	case hpaplusv1alpha1.DecisionMinimum:
		min := int32(0)
		for i, predictedReplica := range predictedReplicas {
			if i == 0 || predictedReplica < min {
				min = predictedReplica
			}
		}
		targetReplicas = min
	case hpaplusv1alpha1.DecisionMean:
		total := int64(0)
		for _, predictedReplica := range predictedReplicas {
			total += int64(predictedReplica)
		}
		if total <= 0 {
			return targetReplicas, nil
		}
		targetReplicas = int32(math.Round(float64(total) / float64(len(predictedReplicas))))
	case hpaplusv1alpha1.DecisionMedian:
		if len(predictedReplicas) <= 0 {
			return targetReplicas, nil
		}
		halfIndex := len(predictedReplicas) / 2
		if len(predictedReplicas)%2 == 0 {
			// Even
			targetReplicas = int32((int64(predictedReplicas[halfIndex-1]) + int64(predictedReplicas[halfIndex])) / 2)
		} else {
			// Odd
			targetReplicas = predictedReplicas[halfIndex]
		}
	default:
		return 0, fmt.Errorf("unknown decision type '%s'", decisionType)
	}

	return targetReplicas, nil
}

func DecideTargetReplicasByBehavior(
	behavior *autoscalingv2.HorizontalPodAutoscalerBehavior, currentReplicas int32, targetReplicas int32,
	minReplicas int32, maxReplicas int32,
	scaleUpReplicaHistory []hpaplusv1alpha1.TimestampedSample,
	scaleDownReplicaHistory []hpaplusv1alpha1.TimestampedSample,
	scaleUpEventHistory []hpaplusv1alpha1.TimestampedSample,
	scaleDownEventHistory []hpaplusv1alpha1.TimestampedSample,
	now time.Time) int32 {

	return DecideTargetReplicasByBehaviorDetails(behavior, currentReplicas, targetReplicas, minReplicas, maxReplicas, scaleUpReplicaHistory, scaleDownReplicaHistory, scaleUpEventHistory, scaleDownEventHistory, now).Desired
}

// BehaviorResult exposes successive decisions without changing policy semantics.
type BehaviorResult struct{ Stabilized, RateLimited, Desired int32 }

func DecideTargetReplicasByBehaviorDetails(
	behavior *autoscalingv2.HorizontalPodAutoscalerBehavior, currentReplicas int32, targetReplicas int32,
	minReplicas int32, maxReplicas int32,
	scaleUpReplicaHistory []hpaplusv1alpha1.TimestampedSample,
	scaleDownReplicaHistory []hpaplusv1alpha1.TimestampedSample,
	scaleUpEventHistory []hpaplusv1alpha1.TimestampedSample,
	scaleDownEventHistory []hpaplusv1alpha1.TimestampedSample,
	now time.Time) BehaviorResult {

	// Upscale stabilization
	upscaleMinValue := targetReplicas
	for _, timestampedReplica := range scaleUpReplicaHistory {
		if timestampedReplica.Replicas < upscaleMinValue {
			upscaleMinValue = timestampedReplica.Replicas
		}
	}

	// Downscale stabilization
	downscaleMaxValue := targetReplicas
	for _, timestampedReplica := range scaleDownReplicaHistory {
		if timestampedReplica.Replicas > downscaleMaxValue {
			downscaleMaxValue = timestampedReplica.Replicas
		}
	}

	stabilizedReplicas := currentReplicas

	if stabilizedReplicas < upscaleMinValue {
		stabilizedReplicas = upscaleMinValue
	}

	if stabilizedReplicas > downscaleMaxValue {
		stabilizedReplicas = downscaleMaxValue
	}

	targetReplicas = decideTargetReplicasByBehaviorRate(behavior, currentReplicas, stabilizedReplicas, scaleUpEventHistory,
		scaleDownEventHistory, now)

	rateLimited := targetReplicas
	if targetReplicas < minReplicas {
		targetReplicas = minReplicas
	}

	if targetReplicas > maxReplicas {
		targetReplicas = maxReplicas
	}

	return BehaviorResult{Stabilized: stabilizedReplicas, RateLimited: rateLimited, Desired: targetReplicas}
}

// returns the longest policy period in seconds from the policies in a set of scaling rules provided
func GetLongestPolicyPeriod(scalingRules *autoscalingv2.HPAScalingRules) int32 {
	var longestPolicyPeriod int32 = 0
	if scalingRules == nil {
		return longestPolicyPeriod
	}

	for _, policy := range scalingRules.Policies {
		if policy.PeriodSeconds > longestPolicyPeriod {
			longestPolicyPeriod = policy.PeriodSeconds
		}
	}
	return longestPolicyPeriod
}

func PruneTimestampedReplicasToWindow(
	timestampedReplicas []hpaplusv1alpha1.TimestampedSample, window int32, now time.Time) []hpaplusv1alpha1.TimestampedSample {

	prunedTimestampedReplicas := []hpaplusv1alpha1.TimestampedSample{}

	// Prune old evaluations
	// Cutoff is current time - stabilization window
	cutoff := now.Add(time.Duration(-window) * time.Second)

	// Add any timestamped replicas after the cut off (inside the window) to the pruned list
	for _, timestampedReplica := range timestampedReplicas {
		if timestampedReplica.Time != nil && timestampedReplica.Time.After(cutoff) {
			prunedTimestampedReplicas = append(prunedTimestampedReplicas, timestampedReplica)
		}
	}

	return prunedTimestampedReplicas
}

func decideTargetReplicasByBehaviorRate(behavior *autoscalingv2.HorizontalPodAutoscalerBehavior,
	currentReplicas int32, targetReplicas int32,
	scaleUpEvents []hpaplusv1alpha1.TimestampedSample,
	scaleDownEvents []hpaplusv1alpha1.TimestampedSample, now time.Time) int32 {

	if targetReplicas > currentReplicas {
		// Scale up
		scaleUpLimit := calculateScaleUpLimitWithinScalingRules(currentReplicas, scaleUpEvents, scaleDownEvents,
			behavior.ScaleUp, now)

		if scaleUpLimit < currentReplicas {
			// We shouldn't scale up further until the scaleUpEvents will be cleaned up
			scaleUpLimit = currentReplicas
		}

		if targetReplicas > scaleUpLimit {
			return scaleUpLimit
		}
	} else if targetReplicas < currentReplicas {
		// Scale down
		scaleDownLimit := calculateScaleDownLimitWithinScalingRules(currentReplicas, scaleUpEvents, scaleDownEvents,
			behavior.ScaleDown, now)

		if scaleDownLimit > currentReplicas {
			// We shouldn't scale down further until the scaleDownEvents will be cleaned up
			scaleDownLimit = currentReplicas
		}

		if targetReplicas < scaleDownLimit {
			return scaleDownLimit
		}
	}

	// Within the scaling limits
	return targetReplicas
}

func calculateScaleUpLimitWithinScalingRules(currentReplicas int32,
	scaleUpEvents []hpaplusv1alpha1.TimestampedSample,
	scaleDownEvents []hpaplusv1alpha1.TimestampedSample,
	scalingRules *autoscalingv2.HPAScalingRules, now time.Time) int32 {
	var result int32
	var proposed int32
	var selectPolicyFn func(int32, int32) int32
	if *scalingRules.SelectPolicy == autoscalingv2.DisabledPolicySelect {
		return currentReplicas // Scaling is disabled
	} else if *scalingRules.SelectPolicy == autoscalingv2.MinChangePolicySelect {
		result = math.MaxInt32
		selectPolicyFn = min // For scaling up, the lowest change ('min' policy) produces a minimum value
	} else {
		result = math.MinInt32
		selectPolicyFn = max // Use the default policy otherwise to produce a highest possible change
	}
	for _, policy := range scalingRules.Policies {
		replicasAddedInCurrentPeriod := getReplicaChanges(scaleUpEvents, policy.PeriodSeconds, now)
		replicasDeletedInCurrentPeriod := getReplicaChanges(scaleDownEvents, policy.PeriodSeconds, now)
		periodStartReplicas := max64(0, int64(currentReplicas)-replicasAddedInCurrentPeriod+replicasDeletedInCurrentPeriod)
		if policy.Type == autoscalingv2.PodsScalingPolicy {
			proposed = boundedReplicas(periodStartReplicas + int64(policy.Value))
		} else if policy.Type == autoscalingv2.PercentScalingPolicy {
			// the proposal has to be rounded up because the proposed change might not increase the replica count causing the target to never scale up
			proposed = boundedReplicas(int64(math.Ceil(math.Min(float64(math.MaxInt32), float64(periodStartReplicas)*(1+float64(policy.Value)/100)))))
		}
		result = selectPolicyFn(result, proposed)
	}
	return result
}

func calculateScaleDownLimitWithinScalingRules(currentReplicas int32,
	scaleUpEvents []hpaplusv1alpha1.TimestampedSample,
	scaleDownEvents []hpaplusv1alpha1.TimestampedSample,
	scalingRules *autoscalingv2.HPAScalingRules, now time.Time) int32 {
	var result int32
	var proposed int32
	var selectPolicyFn func(int32, int32) int32
	if *scalingRules.SelectPolicy == autoscalingv2.DisabledPolicySelect {
		return currentReplicas // Scaling is disabled
	} else if *scalingRules.SelectPolicy == autoscalingv2.MinChangePolicySelect {
		result = math.MinInt32
		selectPolicyFn = max // For scaling down, the lowest change ('min' policy) produces a maximum value
	} else {
		result = math.MaxInt32
		selectPolicyFn = min // Use the default policy otherwise to produce a highest possible change
	}
	for _, policy := range scalingRules.Policies {
		replicasAddedInCurrentPeriod := getReplicaChanges(scaleUpEvents, policy.PeriodSeconds, now)
		replicasDeletedInCurrentPeriod := getReplicaChanges(scaleDownEvents, policy.PeriodSeconds, now)
		periodStartReplicas := max64(0, int64(currentReplicas)-replicasAddedInCurrentPeriod+replicasDeletedInCurrentPeriod)
		if policy.Type == autoscalingv2.PodsScalingPolicy {
			proposed = boundedReplicas(periodStartReplicas - int64(policy.Value))
		} else if policy.Type == autoscalingv2.PercentScalingPolicy {
			proposed = boundedReplicas(int64(math.Max(0, math.Min(float64(math.MaxInt32), float64(periodStartReplicas)*(1-float64(policy.Value)/100)))))
		}
		result = selectPolicyFn(result, proposed)
	}
	return result
}

func getReplicaChanges(scaleEvents []hpaplusv1alpha1.TimestampedSample, periodSeconds int32, now time.Time) int64 {
	period := time.Second * time.Duration(periodSeconds)
	cutoff := now.Add(-period)
	var replicas int64
	for _, scaleEvent := range scaleEvents {
		if scaleEvent.Time != nil && scaleEvent.Time.After(cutoff) {
			replicas += int64(scaleEvent.Replicas)
		}
	}
	return replicas
}

func boundedReplicas(value int64) int32 {
	if value <= 0 {
		return 0
	}
	if value >= math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(value)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func max(a, b int32) int32 {
	if a >= b {
		return a
	}
	return b
}

func min(a, b int32) int32 {
	if a <= b {
		return a
	}
	return b
}
