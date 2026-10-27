package metrics

import (
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
)

func requestedResources(specs []autoscalingv2.MetricSpec) map[corev1.ResourceName]bool {
	names := map[corev1.ResourceName]bool{}
	for _, spec := range specs {
		if spec.Resource != nil && spec.Resource.Target.AverageUtilization != nil {
			names[spec.Resource.Name] = true
		}
	}
	return names
}
