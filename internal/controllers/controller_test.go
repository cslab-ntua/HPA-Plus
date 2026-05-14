package controllers

import (
	"testing"

	jamiethompsonmev1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
)

func TestRequiredHistorySize(t *testing.T) {
	arimaHistory := 12
	tests := []struct {
		name     string
		model    jamiethompsonmev1alpha1.Model
		expected int
	}{
		{
			name: "linear",
			model: jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeLinear,
				Linear: &jamiethompsonmev1alpha1.Linear{
					HistorySize: 6,
				},
			},
			expected: 6,
		},
		{
			name: "xgboost",
			model: jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeXGBoost,
				XGBoost: &jamiethompsonmev1alpha1.XGBoost{
					HistorySize: 10,
				},
			},
			expected: 10,
		},
		{
			name: "lightgbm",
			model: jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeLightGBM,
				LightGBM: &jamiethompsonmev1alpha1.LightGBM{
					HistorySize: 14,
				},
			},
			expected: 14,
		},
		{
			name: "arima-with-history",
			model: jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeArima,
				Arima: &jamiethompsonmev1alpha1.Arima{
					HistorySize: &arimaHistory,
				},
			},
			expected: arimaHistory,
		},
		{
			name: "arima-default-history",
			model: jamiethompsonmev1alpha1.Model{
				Type:  jamiethompsonmev1alpha1.TypeArima,
				Arima: &jamiethompsonmev1alpha1.Arima{},
			},
			expected: defaultArimaHistorySize,
		},
		{
			name: "holtWinters",
			model: jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeHoltWinters,
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					SeasonalPeriods: 3,
					StoredSeasons:   4,
				},
			},
			expected: 12,
		},
		{
			name: "unknown-type",
			model: jamiethompsonmev1alpha1.Model{
				Type: "unknown",
			},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := requiredHistorySize(&tt.model); got != tt.expected {
				t.Fatalf("requiredHistorySize() = %d, want %d", got, tt.expected)
			}
		})
	}
}

func TestModelHasSufficientHistory(t *testing.T) {
	tests := []struct {
		name    string
		model   jamiethompsonmev1alpha1.Model
		history int
		want    bool
	}{
		{
			name: "enough-history",
			model: jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeLinear,
				Linear: &jamiethompsonmev1alpha1.Linear{
					HistorySize: 3,
				},
			},
			history: 3,
			want:    true,
		},
		{
			name: "holtwinters-cpu-history",
			model: jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeHoltWinters,
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					SeasonalPeriods: 2,
					StoredSeasons:   2,
				},
			},
			history: 3,
			want:    false,
		},
		{
			name: "insufficient-history",
			model: jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeXGBoost,
				XGBoost: &jamiethompsonmev1alpha1.XGBoost{
					HistorySize: 5,
				},
			},
			history: 4,
			want:    false,
		},
		{
			name: "lightgbm-cpu-history",
			model: jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeLightGBM,
				LightGBM: &jamiethompsonmev1alpha1.LightGBM{
					HistorySize: 3,
				},
			},
			history: 2,
			want:    false,
		},
		{
			name: "no-requirement",
			model: jamiethompsonmev1alpha1.Model{
				Type: "unknown",
			},
			history: 0,
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			history := make([]jamiethompsonmev1alpha1.TimestampedReplicas, tt.history)
			if prediction.UsesCPUHistory(tt.model.Type) {
				for i := range history {
					value := int64(i + 1)
					history[i].TotalCPUUsageMillicores = &value
				}
			}
			if got := modelHasSufficientHistory(&tt.model, history); got != tt.want {
				t.Fatalf("modelHasSufficientHistory() = %v, want %v", got, tt.want)
			}
		})
	}
}
