package metrics

import (
	"fmt"
	"math"

	gathered "github.com/jthomperoo/k8shorizmetrics/v2/metrics"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
)

// ValidateResourceUtilization bounds inputs to the legacy evaluator's int64
// sums, percentage multiplication and int32 utilization/replica conversion.
// It includes a conservative allowance for the evaluator's missing-pod request
// substitution. Out-of-range observations are unavailable, never wrapped demand.
func ValidateResourceUtilization(metric *gathered.Metric) error {
	if metric == nil {
		return fmt.Errorf("missing gathered metric")
	}
	if metric.Spec.Type != autoscalingv2.ResourceMetricSourceType {
		return nil
	}
	if metric.Spec.Resource == nil || metric.Resource == nil {
		return fmt.Errorf("missing resource metric data")
	}
	if metric.Spec.Resource.Target.AverageUtilization == nil {
		return nil // AverageValue and other metric families have different arithmetic.
	}
	target := *metric.Spec.Resource.Target.AverageUtilization
	if target <= 0 {
		return fmt.Errorf("resource utilization target must be positive")
	}
	r := metric.Resource
	if r.ReadyPodCount < 0 || r.ReadyPodCount > math.MaxInt32 || r.TotalPods < 0 {
		return fmt.Errorf("resource metric has invalid pod counts")
	}
	const limit = int64(math.MaxInt64 / 100)
	allRequests, allUsage := int64(0), int64(0)
	for _, request := range r.Requests {
		if request <= 0 || request > limit-allRequests {
			return fmt.Errorf("resource requests must be positive and within the utilization arithmetic limit")
		}
		allRequests += request
	}
	matchedUsage, matchedRequests := int64(0), int64(0)
	for name, observation := range r.PodMetricsInfo {
		if observation.Value < 0 || observation.Value > limit-allUsage {
			return fmt.Errorf("resource usage must be nonnegative and within the utilization arithmetic limit")
		}
		allUsage += observation.Value
		if request, ok := r.Requests[name]; ok {
			matchedUsage += observation.Value
			matchedRequests += request
		}
	}
	if len(r.MissingPods) > 0 && allUsage > limit-allRequests {
		return fmt.Errorf("resource usage with missing-pod substitution exceeds the utilization arithmetic limit")
	}
	if matchedRequests == 0 {
		return nil // The evaluator reports unavailable metrics when no pods match.
	}
	utilization := matchedUsage * 100 / matchedRequests
	if utilization > math.MaxInt32 {
		return fmt.Errorf("resource utilization exceeds int32")
	}
	// Missing/unready-pod substitution can only move utilization toward 100%
	// or zero. Bound both the initial and substituted replica calculations.
	if utilization < 100 && len(r.MissingPods) > 0 {
		utilization = 100
	}
	count := int64(len(r.PodMetricsInfo)) + int64(len(r.MissingPods)) + int64(len(r.IgnoredPods))
	if r.ReadyPodCount > count {
		count = r.ReadyPodCount
	}
	if count > math.MaxInt32 || utilization*count > int64(math.MaxInt32)*int64(target) {
		return fmt.Errorf("resource replica evaluation exceeds int32")
	}
	return nil
}
