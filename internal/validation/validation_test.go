package validation

import (
	"strings"
	"testing"

	jamiethompsonmev1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
)

func int32Ptr(v int32) *int32 {
	return &v
}

func intPtr(v int) *int {
	return &v
}

func float64Ptr(v float64) *float64 {
	return &v
}

func makeCPUUtilizationMetric(target int32) autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricSource{
			Name: corev1.ResourceCPU,
			Target: autoscalingv2.MetricTarget{
				Type:               autoscalingv2.UtilizationMetricType,
				AverageUtilization: int32Ptr(target),
			},
		},
	}
}

func makeInstance(model jamiethompsonmev1alpha1.Model) *jamiethompsonmev1alpha1.PredictiveHorizontalPodAutoscaler {
	return &jamiethompsonmev1alpha1.PredictiveHorizontalPodAutoscaler{
		Spec: jamiethompsonmev1alpha1.PredictiveHorizontalPodAutoscalerSpec{
			MaxReplicas: 10,
			Metrics:     []autoscalingv2.MetricSpec{makeCPUUtilizationMetric(70)},
			Models:      []jamiethompsonmev1alpha1.Model{model},
		},
	}
}

func TestValidateRejectsInvalidTreeModelParameters(t *testing.T) {
	tests := []struct {
		name    string
		model   jamiethompsonmev1alpha1.Model
		wantErr string
	}{
		{
			name: "xgboost-zero-subsample",
			model: jamiethompsonmev1alpha1.Model{
				Name: "xb",
				Type: jamiethompsonmev1alpha1.TypeXGBoost,
				XGBoost: &jamiethompsonmev1alpha1.XGBoost{
					HistorySize: 10,
					LookAhead:   1000,
					Lags:        4,
					Subsample:   float64Ptr(0),
				},
			},
			wantErr: "XGBoost subsample must be in (0, 1]",
		},
		{
			name: "xgboost-zero-colsample",
			model: jamiethompsonmev1alpha1.Model{
				Name: "xb",
				Type: jamiethompsonmev1alpha1.TypeXGBoost,
				XGBoost: &jamiethompsonmev1alpha1.XGBoost{
					HistorySize:     10,
					LookAhead:       1000,
					Lags:            4,
					ColsampleBytree: float64Ptr(0),
				},
			},
			wantErr: "XGBoost colsampleBytree must be in (0, 1]",
		},
		{
			name: "lightgbm-zero-subsample",
			model: jamiethompsonmev1alpha1.Model{
				Name: "lgb",
				Type: jamiethompsonmev1alpha1.TypeLightGBM,
				LightGBM: &jamiethompsonmev1alpha1.LightGBM{
					HistorySize: 10,
					LookAhead:   1000,
					Lags:        4,
					Subsample:   float64Ptr(0),
				},
			},
			wantErr: "LightGBM subsample must be in (0, 1]",
		},
		{
			name: "lightgbm-zero-colsample",
			model: jamiethompsonmev1alpha1.Model{
				Name: "lgb",
				Type: jamiethompsonmev1alpha1.TypeLightGBM,
				LightGBM: &jamiethompsonmev1alpha1.LightGBM{
					HistorySize:     10,
					LookAhead:       1000,
					Lags:            4,
					ColsampleBytree: float64Ptr(0),
				},
			},
			wantErr: "LightGBM colsampleBytree must be in (0, 1]",
		},
		{
			name: "lightgbm-zero-learning-rate",
			model: jamiethompsonmev1alpha1.Model{
				Name: "lgb",
				Type: jamiethompsonmev1alpha1.TypeLightGBM,
				LightGBM: &jamiethompsonmev1alpha1.LightGBM{
					HistorySize:  10,
					LookAhead:    1000,
					Lags:         4,
					LearningRate: float64Ptr(0),
				},
			},
			wantErr: "LightGBM learningRate must be > 0",
		},
		{
			name: "lightgbm-negative-reg-lambda",
			model: jamiethompsonmev1alpha1.Model{
				Name: "lgb",
				Type: jamiethompsonmev1alpha1.TypeLightGBM,
				LightGBM: &jamiethompsonmev1alpha1.LightGBM{
					HistorySize: 10,
					LookAhead:   1000,
					Lags:        4,
					RegLambda:   float64Ptr(-1),
				},
			},
			wantErr: "LightGBM regLambda must be >= 0",
		},
		{
			name: "lightgbm-negative-reg-alpha",
			model: jamiethompsonmev1alpha1.Model{
				Name: "lgb",
				Type: jamiethompsonmev1alpha1.TypeLightGBM,
				LightGBM: &jamiethompsonmev1alpha1.LightGBM{
					HistorySize: 10,
					LookAhead:   1000,
					Lags:        4,
					RegAlpha:    float64Ptr(-1),
				},
			},
			wantErr: "LightGBM regAlpha must be >= 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(makeInstance(tt.model))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestValidateAllowsZeroLightGBMRegularization(t *testing.T) {
	instance := makeInstance(jamiethompsonmev1alpha1.Model{
		Name: "lgb",
		Type: jamiethompsonmev1alpha1.TypeLightGBM,
		LightGBM: &jamiethompsonmev1alpha1.LightGBM{
			HistorySize: 10,
			LookAhead:   1000,
			Lags:        4,
			RegLambda:   float64Ptr(0),
			RegAlpha:    float64Ptr(0),
		},
	})

	if err := Validate(instance); err != nil {
		t.Fatalf("expected zero regularization to be valid, got %v", err)
	}
}

func TestValidateAllowsLightGBMUnboundedMaxDepth(t *testing.T) {
	instance := makeInstance(jamiethompsonmev1alpha1.Model{
		Name: "lgb",
		Type: jamiethompsonmev1alpha1.TypeLightGBM,
		LightGBM: &jamiethompsonmev1alpha1.LightGBM{
			HistorySize: 10,
			LookAhead:   1000,
			Lags:        4,
			MaxDepth:    intPtr(-1),
		},
	})

	if err := Validate(instance); err != nil {
		t.Fatalf("expected maxDepth=-1 to be valid, got %v", err)
	}
}
