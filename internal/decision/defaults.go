package decision

import (
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
)

const defaultDecisionType = api.DefaultDecisionType
const defaultMinReplicas = api.DefaultMinReplicas

func BehaviorDefaults(behavior *autoscalingv2.HorizontalPodAutoscalerBehavior) *autoscalingv2.HorizontalPodAutoscalerBehavior {
	return api.DefaultBehavior(behavior)
}
func int32Ptr(v int32) *int32 { return &v }
