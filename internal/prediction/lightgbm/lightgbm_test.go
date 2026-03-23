package lightgbm_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	jamiethompsonmev1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/fake"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction/lightgbm"
	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func intPtr(i int) *int {
	return &i
}

func int64Ptr(i int64) *int64 {
	return &i
}

func float64Ptr(f float64) *float64 {
	return &f
}

func attachCPUUsage(history []jamiethompsonmev1alpha1.TimestampedReplicas) []jamiethompsonmev1alpha1.TimestampedReplicas {
	result := make([]jamiethompsonmev1alpha1.TimestampedReplicas, len(history))
	for i, entry := range history {
		result[i] = entry
		if result[i].TotalCPUUsageMillicores == nil {
			value := int64(result[i].Replicas)
			result[i].TotalCPUUsageMillicores = &value
		}
	}
	return result
}

func TestPredict_GetPredictionResult(t *testing.T) {
	equateErrorMessage := cmp.Comparer(func(x, y error) bool {
		if x == nil || y == nil {
			return x == nil && y == nil
		}
		return x.Error() == y.Error()
	})

	tests := []struct {
		description    string
		expected       int32
		expectedErr    error
		predicter      *lightgbm.Predict
		model          *jamiethompsonmev1alpha1.Model
		replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas
	}{
		{
			description:    "Fail no LightGBM configuration",
			expected:       0,
			expectedErr:    errors.New("no LightGBM configuration provided for model"),
			predicter:      &lightgbm.Predict{},
			model:          &jamiethompsonmev1alpha1.Model{},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{},
		},
		{
			description:    "Fail no evaluations",
			expected:       0,
			expectedErr:    errors.New("no CPU usage evaluations provided for LightGBM model"),
			predicter:      &lightgbm.Predict{},
			model:          &jamiethompsonmev1alpha1.Model{Type: jamiethompsonmev1alpha1.TypeLightGBM, LightGBM: &jamiethompsonmev1alpha1.LightGBM{HistorySize: 5, LookAhead: 1000, Lags: 2}},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{},
		},
		{
			description: "Success single CPU evaluation returns last replicas",
			expected:    6,
			expectedErr: nil,
			predicter:   &lightgbm.Predict{},
			model: &jamiethompsonmev1alpha1.Model{
				Type:     jamiethompsonmev1alpha1.TypeLightGBM,
				LightGBM: &jamiethompsonmev1alpha1.LightGBM{HistorySize: 5, LookAhead: 1000, Lags: 2},
			},
			replicaHistory: attachCPUUsage([]jamiethompsonmev1alpha1.TimestampedReplicas{
				{Replicas: 6},
			}),
		},
		{
			description: "Success converts predicted CPU usage to replicas",
			expected:    4,
			expectedErr: nil,
			predicter: &lightgbm.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "2240", nil
					},
				},
			},
			model: &jamiethompsonmev1alpha1.Model{
				Type:                           jamiethompsonmev1alpha1.TypeLightGBM,
				CPURequestPerPodMillicores:     800,
				TargetCPUUtilizationPercentage: 70,
				LightGBM: &jamiethompsonmev1alpha1.LightGBM{
					HistorySize: 5,
					LookAhead:   1000,
					Lags:        2,
					WindowSize:  intPtr(2),
				},
			},
			replicaHistory: attachCPUUsage([]jamiethompsonmev1alpha1.TimestampedReplicas{
				{Replicas: 2, Time: &metav1.Time{Time: time.Time{}.Add(1 * time.Second)}},
				{Replicas: 3, Time: &metav1.Time{Time: time.Time{}.Add(2 * time.Second)}},
				{Replicas: 4, Time: &metav1.Time{Time: time.Time{}.Add(3 * time.Second)}},
			}),
		},
		{
			description: "Fail missing CPU request per pod",
			expected:    0,
			expectedErr: errors.New("missing CPU request per pod for LightGBM CPU-history prediction"),
			predicter: &lightgbm.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "2240", nil
					},
				},
			},
			model: &jamiethompsonmev1alpha1.Model{
				Type:                           jamiethompsonmev1alpha1.TypeLightGBM,
				TargetCPUUtilizationPercentage: 70,
				LightGBM: &jamiethompsonmev1alpha1.LightGBM{
					HistorySize: 5,
					LookAhead:   1000,
					Lags:        2,
				},
			},
			replicaHistory: attachCPUUsage([]jamiethompsonmev1alpha1.TimestampedReplicas{
				{Replicas: 2, Time: &metav1.Time{Time: time.Time{}.Add(1 * time.Second)}},
				{Replicas: 3, Time: &metav1.Time{Time: time.Time{}.Add(2 * time.Second)}},
			}),
		},
		{
			description: "Fail missing target CPU utilization",
			expected:    0,
			expectedErr: errors.New("missing target CPU utilization for LightGBM CPU-history prediction"),
			predicter: &lightgbm.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "2240", nil
					},
				},
			},
			model: &jamiethompsonmev1alpha1.Model{
				Type:                       jamiethompsonmev1alpha1.TypeLightGBM,
				CPURequestPerPodMillicores: 800,
				LightGBM: &jamiethompsonmev1alpha1.LightGBM{
					HistorySize: 5,
					LookAhead:   1000,
					Lags:        2,
				},
			},
			replicaHistory: attachCPUUsage([]jamiethompsonmev1alpha1.TimestampedReplicas{
				{Replicas: 2, Time: &metav1.Time{Time: time.Time{}.Add(1 * time.Second)}},
				{Replicas: 3, Time: &metav1.Time{Time: time.Time{}.Add(2 * time.Second)}},
			}),
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			result, err := test.predicter.GetPrediction(test.model, test.replicaHistory)
			if !cmp.Equal(err, test.expectedErr, equateErrorMessage) {
				t.Fatalf("unexpected error (-want +got):\n%s", cmp.Diff(test.expectedErr, err, equateErrorMessage))
			}
			if result != test.expected {
				t.Fatalf("expected %d, got %d", test.expected, result)
			}
		})
	}
}

func TestPredict_GetPredictionResult_PreservesZeroRegularizationValues(t *testing.T) {
	p := &lightgbm.Predict{
		Runner: &fake.Run{
			RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
				payload := map[string]interface{}{}
				if err := json.Unmarshal([]byte(value), &payload); err != nil {
					t.Fatalf("failed to decode payload: %v", err)
				}
				if got := payload["regLambda"]; got != float64(0) {
					t.Fatalf("expected regLambda 0, got %#v", got)
				}
				if got := payload["regAlpha"]; got != float64(0) {
					t.Fatalf("expected regAlpha 0, got %#v", got)
				}
				return "2240", nil
			},
		},
	}

	model := &jamiethompsonmev1alpha1.Model{
		Type:                           jamiethompsonmev1alpha1.TypeLightGBM,
		CPURequestPerPodMillicores:     800,
		TargetCPUUtilizationPercentage: 70,
		LightGBM: &jamiethompsonmev1alpha1.LightGBM{
			HistorySize: 5,
			LookAhead:   1000,
			Lags:        2,
			RegLambda:   float64Ptr(0),
			RegAlpha:    float64Ptr(0),
		},
	}

	history := attachCPUUsage([]jamiethompsonmev1alpha1.TimestampedReplicas{
		{Replicas: 2, Time: &metav1.Time{Time: time.Time{}.Add(1 * time.Second)}},
		{Replicas: 3, Time: &metav1.Time{Time: time.Time{}.Add(2 * time.Second)}},
		{Replicas: 4, Time: &metav1.Time{Time: time.Time{}.Add(3 * time.Second)}},
	})

	result, err := p.GetPredictionResult(model, history)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Replicas != 4 {
		t.Fatalf("expected replicas 4, got %d", result.Replicas)
	}
}

func TestPredict_GetPredictionResult_ConsumedUntilUsesTrainingHistory(t *testing.T) {
	p := &lightgbm.Predict{
		Runner: &fake.Run{
			RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
				return "2240", nil
			},
		},
	}

	model := &jamiethompsonmev1alpha1.Model{
		Type:                           jamiethompsonmev1alpha1.TypeLightGBM,
		CPURequestPerPodMillicores:     800,
		TargetCPUUtilizationPercentage: 70,
		LightGBM: &jamiethompsonmev1alpha1.LightGBM{
			HistorySize: 5,
			LookAhead:   1000,
			Lags:        2,
			WindowSize:  intPtr(2),
		},
	}

	history := []jamiethompsonmev1alpha1.TimestampedReplicas{
		{Replicas: 2, Time: &metav1.Time{Time: time.Time{}.Add(1 * time.Second)}, TotalCPUUsageMillicores: int64Ptr(1120)},
		{Replicas: 3, Time: &metav1.Time{Time: time.Time{}.Add(2 * time.Second)}, TotalCPUUsageMillicores: int64Ptr(1680)},
		{Replicas: 4, Time: &metav1.Time{Time: time.Time{}.Add(3 * time.Second)}, TotalCPUUsageMillicores: int64Ptr(2240)},
		{Replicas: 5, Time: &metav1.Time{Time: time.Time{}.Add(4 * time.Second)}},
	}

	result, err := p.GetPredictionResult(model, history)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Replicas != 4 {
		t.Fatalf("expected replicas 4, got %d", result.Replicas)
	}
	if diff := cmp.Diff(prediction.LatestTimestamp(history[:3]), result.ConsumedUntil); diff != "" {
		t.Fatalf("unexpected consumed timestamp (-want +got):\n%s", diff)
	}
}
