package metrics

import (
	"fmt"
	"math"

	gathered "github.com/jthomperoo/k8shorizmetrics/v2/metrics"
	"github.com/jthomperoo/k8shorizmetrics/v2/metrics/podmetrics"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"
)

// milliValue checks before conversion: Quantity.MilliValue may wrap even when
// its original value was a valid, positive Kubernetes quantity.
func milliValue(quantity *resource.Quantity, positive bool) (int64, error) {
	if quantity == nil || quantity.Sign() < 0 || (positive && quantity.Sign() == 0) {
		return 0, fmt.Errorf("missing or invalid metric quantity")
	}
	maximum := resource.NewMilliQuantity(math.MaxInt64, resource.DecimalSI)
	if quantity.Cmp(*maximum) > 0 {
		return 0, fmt.Errorf("metric quantity exceeds int64 milli-units")
	}
	return quantity.MilliValue(), nil
}

func addMetric(total, value int64) (int64, error) {
	if value < 0 || value > math.MaxInt64-total {
		return 0, fmt.Errorf("negative or overflowing metric sum")
	}
	return total + value, nil
}

// ValidateMetric bounds the arithmetic used by all supported legacy evaluators.
// Values outside these bounds are unavailable rather than wrapped or fabricated.
func ValidateMetric(metric *gathered.Metric, current int32) error {
	if metric == nil || current < 0 {
		return fmt.Errorf("missing metric or invalid replica count")
	}
	switch metric.Spec.Type {
	case autoscalingv2.ResourceMetricSourceType:
		if err := ValidateResourceUtilization(metric); err != nil {
			return err
		}
		if metric.Spec.Resource.Target.AverageUtilization != nil {
			return nil
		}
		m := metric.Resource
		return validatePlain(m.PodMetricsInfo, m.ReadyPodCount, m.MissingPods, m.IgnoredPods, metric.Spec.Resource.Target.AverageValue)
	case autoscalingv2.PodsMetricSourceType:
		if metric.Pods == nil || metric.Spec.Pods == nil {
			return fmt.Errorf("missing pod metric data")
		}
		m := metric.Pods
		return validatePlain(m.PodMetricsInfo, m.ReadyPodCount, m.MissingPods, m.IgnoredPods, metric.Spec.Pods.Target.AverageValue)
	case autoscalingv2.ObjectMetricSourceType:
		if metric.Object == nil || metric.Spec.Object == nil {
			return fmt.Errorf("missing object metric data")
		}
		m, target := metric.Object, metric.Spec.Object.Target
		if target.Type == autoscalingv2.ValueMetricType {
			return validateScalar(m.Current.Value, target.Value, m.ReadyPodCount, current, false)
		}
		if target.Type == autoscalingv2.AverageValueMetricType {
			return validateScalar(m.Current.AverageValue, target.AverageValue, nil, current, true)
		}
	case autoscalingv2.ExternalMetricSourceType:
		if metric.External == nil || metric.Spec.External == nil {
			return fmt.Errorf("missing external metric data")
		}
		m, target := metric.External, metric.Spec.External.Target
		if target.AverageValue != nil {
			return validateScalar(m.Current.AverageValue, target.AverageValue, nil, current, true)
		}
		if target.Value != nil {
			return validateScalar(m.Current.Value, target.Value, m.ReadyPodCount, current, false)
		}
	}
	return fmt.Errorf("unsupported metric target")
}

func validateScalar(value *int64, quantity *resource.Quantity, ready *int64, current int32, average bool) error {
	target, err := milliValue(quantity, true)
	if err != nil {
		return err
	}
	if value == nil || *value < 0 {
		return fmt.Errorf("missing or negative metric value")
	}
	multiplier := int64(1)
	if !average {
		if ready == nil || *ready < 0 || *ready > math.MaxInt32 {
			return fmt.Errorf("invalid ready pod count")
		}
		if current != 0 {
			multiplier = *ready
		}
	}
	return replicaProductSafe(float64(*value)/float64(target), multiplier)
}

func replicaProductSafe(ratio float64, count int64) error {
	result := math.Ceil(ratio * float64(count))
	if math.IsNaN(result) || math.IsInf(result, 0) || result < 0 || result > math.MaxInt32 {
		return fmt.Errorf("metric replica evaluation exceeds int32")
	}
	return nil
}

func validatePlain(values podmetrics.MetricsInfo, ready int64, missing, ignored sets.String, quantity *resource.Quantity) error {
	target, err := milliValue(quantity, true)
	if err != nil {
		return err
	}
	if len(values) == 0 {
		return fmt.Errorf("no per-pod metric observations")
	}
	if ready < 0 || ready > math.MaxInt32 {
		return fmt.Errorf("invalid ready pod count")
	}
	total := int64(0)
	for _, value := range values {
		total, err = addMetric(total, value.Value)
		if err != nil {
			return err
		}
	}
	average := total / int64(len(values))
	// The dependency substitutes target-valued missing observations on downscale.
	// Bound that sum as well as both the initial and substituted replica counts.
	if average < target && len(missing) > 0 {
		if int64(len(missing)) > (math.MaxInt64-total)/target {
			return fmt.Errorf("missing-pod metric substitution overflows")
		}
		average = target
	}
	count := int64(len(values)) + int64(len(missing)) + int64(len(ignored))
	if ready > count {
		count = ready
	}
	if count > math.MaxInt32 {
		return fmt.Errorf("invalid metric pod count")
	}
	return replicaProductSafe(float64(average)/float64(target), count)
}
