package validation

import (
	"strings"
	"testing"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
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

func makeInstance(model hpaplusv1alpha1.Model) *hpaplusv1alpha1.HPAPlus {
	return &hpaplusv1alpha1.HPAPlus{
		Spec: hpaplusv1alpha1.HPAPlusSpec{
			MaxReplicas: 10,
			Metrics:     []autoscalingv2.MetricSpec{makeCPUUtilizationMetric(70)},
			Models:      []hpaplusv1alpha1.Model{model},
		},
	}
}

func makeInstanceWithoutMetrics(model hpaplusv1alpha1.Model) *hpaplusv1alpha1.HPAPlus {
	return &hpaplusv1alpha1.HPAPlus{
		Spec: hpaplusv1alpha1.HPAPlusSpec{
			MaxReplicas: 10,
			Models:      []hpaplusv1alpha1.Model{model},
		},
	}
}

func TestValidateRejectsInvalidTreeModelParameters(t *testing.T) {
	tests := []struct {
		name    string
		model   hpaplusv1alpha1.Model
		wantErr string
	}{
		{
			name: "xgboost-zero-subsample",
			model: hpaplusv1alpha1.Model{
				Name: "xb",
				Type: hpaplusv1alpha1.TypeXGBoost,
				XGBoost: &hpaplusv1alpha1.XGBoost{
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
			model: hpaplusv1alpha1.Model{
				Name: "xb",
				Type: hpaplusv1alpha1.TypeXGBoost,
				XGBoost: &hpaplusv1alpha1.XGBoost{
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
			model: hpaplusv1alpha1.Model{
				Name: "lgb",
				Type: hpaplusv1alpha1.TypeLightGBM,
				LightGBM: &hpaplusv1alpha1.LightGBM{
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
			model: hpaplusv1alpha1.Model{
				Name: "lgb",
				Type: hpaplusv1alpha1.TypeLightGBM,
				LightGBM: &hpaplusv1alpha1.LightGBM{
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
			model: hpaplusv1alpha1.Model{
				Name: "lgb",
				Type: hpaplusv1alpha1.TypeLightGBM,
				LightGBM: &hpaplusv1alpha1.LightGBM{
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
			model: hpaplusv1alpha1.Model{
				Name: "lgb",
				Type: hpaplusv1alpha1.TypeLightGBM,
				LightGBM: &hpaplusv1alpha1.LightGBM{
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
			model: hpaplusv1alpha1.Model{
				Name: "lgb",
				Type: hpaplusv1alpha1.TypeLightGBM,
				LightGBM: &hpaplusv1alpha1.LightGBM{
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
	instance := makeInstance(hpaplusv1alpha1.Model{
		Name: "lgb",
		Type: hpaplusv1alpha1.TypeLightGBM,
		LightGBM: &hpaplusv1alpha1.LightGBM{
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
	instance := makeInstance(hpaplusv1alpha1.Model{
		Name: "lgb",
		Type: hpaplusv1alpha1.TypeLightGBM,
		LightGBM: &hpaplusv1alpha1.LightGBM{
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

func TestValidateRejectsCPUHistoryModelsWithoutCPUUtilizationMetric(t *testing.T) {
	tests := []struct {
		name    string
		model   hpaplusv1alpha1.Model
		wantErr string
	}{
		{
			name: "linear",
			model: hpaplusv1alpha1.Model{
				Name: "linear",
				Type: hpaplusv1alpha1.TypeLinear,
				Linear: &hpaplusv1alpha1.Linear{
					HistorySize: 4,
					LookAhead:   1000,
				},
			},
			wantErr: "Linear CPU-history prediction requires a CPU resource metric with averageUtilization configured",
		},
		{
			name: "holtwinters",
			model: hpaplusv1alpha1.Model{
				Name: "hw",
				Type: hpaplusv1alpha1.TypeHoltWinters,
				HoltWinters: &hpaplusv1alpha1.HoltWinters{
					Alpha:           float64Ptr(0.3),
					Beta:            float64Ptr(0.2),
					Gamma:           float64Ptr(0.1),
					SeasonalPeriods: 2,
					StoredSeasons:   3,
					Trend:           "add",
					Seasonal:        "add",
				},
			},
			wantErr: "Holt-Winters CPU-history prediction requires a CPU resource metric with averageUtilization configured",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(makeInstanceWithoutMetrics(tt.model))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}
