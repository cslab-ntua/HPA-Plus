package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	metricadapter "github.com/cslab-ntua/HPA-Plus/internal/metrics"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	gathered "github.com/jthomperoo/k8shorizmetrics/v2/metrics"
	"github.com/jthomperoo/k8shorizmetrics/v2/metricsclient"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func hasCPUHistoryModel(instance *api.HPAPlus) bool {
	for _, model := range instance.Spec.Models {
		if prediction.UsesCPUHistory(model.Type) {
			return true
		}
	}
	return false
}

func completeCPUHistory(metric *gathered.Metric, replicas int32) bool {
	if metric == nil || metric.Resource == nil || metricadapter.ValidateMetric(metric, replicas) != nil {
		return false
	}
	r := metric.Resource
	if len(r.MissingPods) > 0 || len(r.IgnoredPods) > 0 || r.TotalPods == 0 || r.TotalPods != len(r.PodMetricsInfo) {
		return false
	}
	usage, _ := aggregateCPUMillicores(r.PodMetricsInfo, r.Requests)
	return usage != nil
}

// Independent training observations do not require a reactive recommendation.
// Only complete, fresh CPU populations are admitted; the scale controller keeps
// using its native metric evaluation and missing-pod protection.
func (r *HPAPlusReconciler) collectIndependentCPU(ctx context.Context, instance *api.HPAPlus,
	scale *autoscalingv1.Scale, provider metricadapter.CPUHistoryGatherer,
) (cpuMetricSnapshot, error) {
	return r.collectCPUWindow(ctx, instance, scale, provider.GatherCPUHistory)
}

// collectRecentCPU measures what the workload used over the most recent
// window, for a floor that must describe demand at decision time.
func (r *HPAPlusReconciler) collectRecentCPU(ctx context.Context, instance *api.HPAPlus,
	scale *autoscalingv1.Scale, provider metricadapter.RecentCPUGatherer, window time.Duration,
) (cpuMetricSnapshot, error) {
	return r.collectCPUWindow(ctx, instance, scale, func(ctx context.Context, spec autoscalingv2.MetricSpec, namespace string, selector labels.Selector) (*gathered.Metric, error) {
		return provider.GatherRecentCPU(ctx, spec, namespace, selector, window)
	})
}

func (r *HPAPlusReconciler) collectCPUWindow(ctx context.Context, instance *api.HPAPlus, scale *autoscalingv1.Scale,
	gather func(context.Context, autoscalingv2.MetricSpec, string, labels.Selector) (*gathered.Metric, error),
) (cpuMetricSnapshot, error) {
	snapshot := cpuMetricSnapshot{TargetIdentity: scaleTargetIdentity(instance, scale)}
	selector, err := labels.Parse(scale.Status.Selector)
	if err != nil || scale.Status.Selector == "" {
		return snapshot, fmt.Errorf("invalid CPU history target selector")
	}
	var maxAge time.Duration
	if instance.Spec.MetricMaxAge != nil {
		maxAge = time.Duration(*instance.Spec.MetricMaxAge) * time.Millisecond
		ctx = metricadapter.WithFreshness(ctx, maxAge, r.now)
	}
	for _, spec := range instance.Spec.Metrics {
		if spec.Resource == nil || spec.Resource.Name != corev1.ResourceCPU || spec.Resource.Target.AverageUtilization == nil {
			continue
		}
		metric, err := gather(ctx, spec, scale.Namespace, selector)
		if err != nil {
			return snapshot, err
		}
		fresh, until := metricadapter.FreshObservations([]*gathered.Metric{metric}, r.now(), maxAge)
		if len(fresh) != 1 || !completeCPUHistory(metric, scale.Status.Replicas) {
			snapshot.MetricsIncomplete = true
			return snapshot, nil
		}
		snapshot.MetricsValidUntil = until
		snapshot.TotalUsageMillicores, snapshot.RequestPerPodMillicores = aggregateCPUMillicores(metric.Resource.PodMetricsInfo, metric.Resource.Requests)
		target := *spec.Resource.Target.AverageUtilization
		snapshot.TargetUtilizationPercentage = &target
		_, utilization, _, err := metricsclient.GetResourceUtilizationRatio(metric.Resource.PodMetricsInfo, metric.Resource.Requests, target)
		if err == nil {
			value := float64(utilization)
			snapshot.Utilization = &value
		}
		request, requestErr := r.getTargetCPURequestPerPod(ctx, instance)
		if requestErr == nil && request != nil && snapshot.RequestPerPodMillicores != nil && *request < *snapshot.RequestPerPodMillicores {
			snapshot.RequestPerPodMillicores = request
		}
		snapshot.SourceID, snapshot.SourceTime = metricadapter.CPUSource(metric, r.now())
		return snapshot, nil
	}
	return snapshot, fmt.Errorf("CPU history requires a CPU utilization metric")
}

// gatherObservations shares short-lived raw inputs between the independent
// sampler and reconciler. Full spec and Scale state prevent reuse after an edit,
// workload replacement, capacity/selector change or scaling write.
func (r *HPAPlusReconciler) gatherObservations(ctx context.Context, instance *api.HPAPlus, scale *autoscalingv1.Scale,
	selector labels.Selector, initialization, readiness, maxAge time.Duration,
) ([]*gathered.Metric, error) {
	gather := func(ctx context.Context) ([]*gathered.Metric, error) {
		values, err := r.Gatherer.Gather(ctx, instance.Spec.Metrics, scale.Namespace, selector, initialization, readiness)
		if err != nil {
			return values, err
		}
		for _, value := range values {
			if err := metricadapter.ValidateMetric(value, scale.Status.Replicas); err != nil {
				return nil, err
			}
		}
		return values, nil
	}
	if r.Observations == nil || instance.UID == "" || scale.UID == "" {
		return gather(ctx)
	}
	encoded, err := json.Marshal(struct {
		UID        string
		Generation int64
		Spec       api.HPAPlusSpec
		Scale      *autoscalingv1.Scale
	}{string(instance.UID), instance.Generation, instance.Spec, scale})
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(encoded)
	ttl := syncPeriodForInstance(instance)
	if ttl > time.Second {
		ttl = time.Second
	}
	return r.Observations.Read(ctx, instance.Namespace+"/"+instance.Name, hex.EncodeToString(hash[:]),
		len(instance.Spec.Metrics), ttl, maxAge, r.now, gather)
}
