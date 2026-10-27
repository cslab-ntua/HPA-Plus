package v1alpha1

import autoscalingv2 "k8s.io/api/autoscaling/v2"

// v1alpha1 runtime defaults are not persisted by Kubernetes admission.
const (
	DefaultMinReplicas              = 1
	DefaultSyncPeriodMillis         = 15000
	DefaultCPUInitializationSeconds = 300
	DefaultInitialReadinessSeconds  = 30
	DefaultTolerance                = 0.1
	DefaultPredictiveTolerance      = 0.1
	DefaultPerSyncPeriod            = 1
	DefaultCalculationTimeoutMillis = 30000
	DefaultARIMAHistorySize         = 50
	DefaultDecisionType             = DecisionMaximum
)

// WithDefaults returns a detached, explicit v1alpha1 configuration. It preserves
// every supplied value (including zero/false/empty), and does not validate it.
// Model-engine choices such as omitted ARIMA trend remain omitted intentionally.
func (s HPAPlusSpec) WithDefaults() HPAPlusSpec {
	out := s.DeepCopy()
	if out.Models == nil {
		out.Models = []Model{}
	}
	if out.Mode == nil {
		v := ModeControl
		out.Mode = &v
	}
	if out.MinReplicas == nil {
		v := int32(DefaultMinReplicas)
		out.MinReplicas = &v
	}
	if out.SyncPeriod == nil {
		v := DefaultSyncPeriodMillis
		out.SyncPeriod = &v
	}
	if out.CPUInitializationPeriod == nil {
		v := DefaultCPUInitializationSeconds
		out.CPUInitializationPeriod = &v
	}
	if out.InitialReadinessDelay == nil {
		v := DefaultInitialReadinessSeconds
		out.InitialReadinessDelay = &v
	}
	if out.Tolerance == nil {
		v := DefaultTolerance
		out.Tolerance = &v
	}
	if out.DecisionType == nil {
		v := DefaultDecisionType
		out.DecisionType = &v
	}
	if out.IncludeHPA == nil {
		v := false
		out.IncludeHPA = &v
	}
	out.Behavior = DefaultBehavior(out.Behavior)
	for i := range out.Models {
		model := &out.Models[i]
		if model.PredictivePolicy != nil && model.PredictivePolicy.Tolerance == nil {
			v := DefaultPredictiveTolerance
			model.PredictivePolicy.Tolerance = &v
		}
		if model.PerSyncPeriod == nil {
			v := DefaultPerSyncPeriod
			model.PerSyncPeriod = &v
		}
		if model.CalculationTimeout == nil {
			v := DefaultCalculationTimeoutMillis
			model.CalculationTimeout = &v
		}
		if model.Arima != nil && model.Arima.HistorySize == nil {
			v := DefaultARIMAHistorySize
			model.Arima.HistorySize = &v
		}
	}
	return *out
}

// Downscale constants
const (
	defaultDownscaleStabilization                 = int32(0)
	defaultDownscalePercentagePolicyPeriodSeconds = int32(60)
	defaultDownscalePercentagePolicyValue         = int32(100)
)

// Upscale constants: Kubernetes' implemented default, double or +4 pods per
// 15 seconds. The API field comment in k8s.io/api still says 60 seconds;
// pkg/apis/autoscaling/v2/defaults.go uses 15.
const (
	defaultUpscaleStabilization                 = int32(0)
	defaultUpscalePercentagePolicyPeriodSeconds = int32(15)
	defaultUpscalePercentagePolicyValue         = int32(100)
	defaultUpscalePodsPolicyPeriodSeconds       = int32(15)
	defaultUpscalePodsPolicyValue               = int32(4)
)

func DefaultBehavior(behavior *autoscalingv2.HorizontalPodAutoscalerBehavior) *autoscalingv2.HorizontalPodAutoscalerBehavior {
	// Rate policies follow these sources; HPA+ defaults both stabilization windows to zero:
	// https://github.com/kubernetes/enhancements/blob/7f681415a0011a0f6f98d9f112eeb7731f9eacd7/keps/sig-autoscaling/853-configurable-hpa-scale-velocity/README.md
	// https://github.com/kubernetes/kubernetes/blob/3e26e104bdf9d0dc3c4046d6350b93557c67f3f4/pkg/apis/autoscaling/v2/defaults.go

	if behavior == nil {
		return &autoscalingv2.HorizontalPodAutoscalerBehavior{
			ScaleDown: defaultDownscale(),
			ScaleUp:   defaultUpscale(),
		}
	}

	// We need to take a deep copy here, since we don't want any defaults we fill in to be persisted on the
	// actual object
	behavior = behavior.DeepCopy()

	behavior.ScaleUp = copyHPAScalingRules(behavior.ScaleUp, defaultUpscale())
	behavior.ScaleDown = copyHPAScalingRules(behavior.ScaleDown, defaultDownscale())

	return behavior
}

// EffectiveTolerance preserves explicit zero while defaulting omitted tolerance.
func (p PredictivePolicy) EffectiveTolerance() float64 {
	if p.Tolerance == nil {
		return DefaultPredictiveTolerance
	}
	return *p.Tolerance
}

func copyHPAScalingRules(from, to *autoscalingv2.HPAScalingRules) *autoscalingv2.HPAScalingRules {
	if from == nil {
		return to
	}
	if from.SelectPolicy != nil {
		to.SelectPolicy = from.SelectPolicy
	}
	if from.StabilizationWindowSeconds != nil {
		to.StabilizationWindowSeconds = from.StabilizationWindowSeconds
	}
	if from.Policies != nil {
		to.Policies = from.Policies
	}
	return to
}

func defaultDownscale() *autoscalingv2.HPAScalingRules {
	return &autoscalingv2.HPAScalingRules{
		StabilizationWindowSeconds: int32Ptr(defaultDownscaleStabilization),
		SelectPolicy:               selectPolicyPtr(autoscalingv2.MaxChangePolicySelect),
		Policies: []autoscalingv2.HPAScalingPolicy{
			{
				Type:          autoscalingv2.PercentScalingPolicy,
				PeriodSeconds: defaultDownscalePercentagePolicyPeriodSeconds,
				Value:         defaultDownscalePercentagePolicyValue,
			},
		},
	}
}

func defaultUpscale() *autoscalingv2.HPAScalingRules {
	return &autoscalingv2.HPAScalingRules{
		StabilizationWindowSeconds: int32Ptr(0),
		SelectPolicy:               selectPolicyPtr(autoscalingv2.MaxChangePolicySelect),
		Policies: []autoscalingv2.HPAScalingPolicy{
			{
				Type:          autoscalingv2.PercentScalingPolicy,
				PeriodSeconds: defaultUpscalePercentagePolicyPeriodSeconds,
				Value:         defaultUpscalePercentagePolicyValue,
			},
			{
				Type:          autoscalingv2.PodsScalingPolicy,
				PeriodSeconds: defaultUpscalePodsPolicyPeriodSeconds,
				Value:         defaultUpscalePodsPolicyValue,
			},
		},
	}
}

func int32Ptr(v int32) *int32 { return &v }
func selectPolicyPtr(v autoscalingv2.ScalingPolicySelect) *autoscalingv2.ScalingPolicySelect {
	return &v
}
