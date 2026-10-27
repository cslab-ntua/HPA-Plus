package metrics

import (
	"encoding/json"

	gathered "github.com/jthomperoo/k8shorizmetrics/v2/metrics"
	"github.com/jthomperoo/k8shorizmetrics/v2/metrics/podmetrics"
	"github.com/jthomperoo/k8shorizmetrics/v2/metricsclient"
	autoscaling "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/resource"
)

// StatusSnapshot describes gathered values before the evaluator adds synthetic
// missing-pod values. Omitted includes unavailable summaries and budget overflow;
// metrics that failed gathering are reported separately by MetricsAvailable.
type StatusSnapshot struct {
	Metrics []autoscaling.MetricStatus
	Omitted int32
}

// Summarize copies at most 64 metric reports and 16 KiB of encoded values. The
// reporting budget never changes which observations are evaluated for scaling.
func Summarize(observations []*gathered.Metric, current int32) StatusSnapshot {
	result := StatusSnapshot{Metrics: make([]autoscaling.MetricStatus, 0)}
	bytes := 2
	for _, observation := range observations {
		if len(result.Metrics) >= 64 {
			result.Omitted++
			continue
		}
		status, ok := summarizeMetric(observation, current)
		if !ok {
			result.Omitted++
			continue
		}
		encoded, err := json.Marshal(status)
		if err != nil || bytes+len(encoded)+1 > 16*1024 {
			result.Omitted++
			continue
		}
		result.Metrics = append(result.Metrics, status)
		bytes += len(encoded) + 1
	}
	return result
}

func summarizeMetric(m *gathered.Metric, current int32) (autoscaling.MetricStatus, bool) {
	status := autoscaling.MetricStatus{}
	if ValidateMetric(m, current) != nil {
		return status, false
	}
	status.Type = m.Spec.Type
	switch m.Spec.Type {
	case autoscaling.ResourceMetricSourceType:
		value := autoscaling.MetricValueStatus{}
		if target := m.Spec.Resource.Target.AverageUtilization; target != nil {
			_, utilization, average, err := metricsclient.GetResourceUtilizationRatio(m.Resource.PodMetricsInfo, m.Resource.Requests, *target)
			if err != nil {
				return status, false
			}
			value.AverageUtilization = &utilization
			value.AverageValue = resource.NewMilliQuantity(average, resource.DecimalSI)
		} else {
			value.AverageValue = averageQuantity(m.Resource.PodMetricsInfo)
		}
		status.Resource = &autoscaling.ResourceMetricStatus{Name: m.Spec.Resource.Name, Current: value}
	case autoscaling.PodsMetricSourceType:
		status.Pods = &autoscaling.PodsMetricStatus{Metric: *m.Spec.Pods.Metric.DeepCopy(),
			Current: autoscaling.MetricValueStatus{AverageValue: averageQuantity(m.Pods.PodMetricsInfo)}}
	case autoscaling.ObjectMetricSourceType:
		value := autoscaling.MetricValueStatus{}
		if m.Spec.Object.Target.Type == autoscaling.AverageValueMetricType {
			value = scalarStatus(nil, m.Object.Current.AverageValue, current)
		} else {
			value = scalarStatus(m.Object.Current.Value, nil, current)
		}
		status.Object = &autoscaling.ObjectMetricStatus{Metric: *m.Spec.Object.Metric.DeepCopy(),
			DescribedObject: m.Spec.Object.DescribedObject, Current: value}
	case autoscaling.ExternalMetricSourceType:
		value := autoscaling.MetricValueStatus{}
		if m.Spec.External.Target.AverageValue != nil {
			value = scalarStatus(nil, m.External.Current.AverageValue, current)
		} else {
			value = scalarStatus(m.External.Current.Value, nil, current)
		}
		status.External = &autoscaling.ExternalMetricStatus{Metric: *m.Spec.External.Metric.DeepCopy(), Current: value}
	}
	return status, true
}

// Inputs have already passed ValidateMetric, including sum/empty-set checks.
func averageQuantity(values podmetrics.MetricsInfo) *resource.Quantity {
	var total int64
	for _, value := range values {
		total += value.Value
	}
	return resource.NewMilliQuantity(total/int64(len(values)), resource.DecimalSI)
}

func scalarStatus(value, total *int64, current int32) autoscaling.MetricValueStatus {
	if total == nil {
		return autoscaling.MetricValueStatus{Value: resource.NewMilliQuantity(*value, resource.DecimalSI)}
	}
	if current == 0 {
		// There is no defined per-pod average yet. Report measured total demand
		// as Value instead of fabricating an AverageValue or dividing by zero.
		return autoscaling.MetricValueStatus{Value: resource.NewMilliQuantity(*total, resource.DecimalSI)}
	}
	average := *total / int64(current)
	if *total%int64(current) != 0 {
		average++ // Round upward to the next milli-unit without total+count overflow.
	}
	return autoscaling.MetricValueStatus{AverageValue: resource.NewMilliQuantity(average, resource.DecimalSI)}
}
