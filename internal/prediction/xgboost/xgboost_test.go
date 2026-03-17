/*
Copyright 2022 The Predictive Horizontal Pod Autoscaler Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package xgboost_test

import (
	"errors"
	"testing"
	"time"

	jamiethompsonmev1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/fake"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction/xgboost"
	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func intPtr(i int) *int {
	return &i
}

func int64Ptr(i int64) *int64 {
	return &i
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
		predicter      *xgboost.Predict
		model          *jamiethompsonmev1alpha1.Model
		replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas
	}{
		{
			description:    "Fail no XGBoost configuration",
			expected:       0,
			expectedErr:    errors.New("no XGBoost configuration provided for model"),
			predicter:      &xgboost.Predict{},
			model:          &jamiethompsonmev1alpha1.Model{},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{},
		},
		{
			description:    "Fail no evaluations",
			expected:       0,
			expectedErr:    errors.New("no CPU usage evaluations provided for XGBoost model"),
			predicter:      &xgboost.Predict{},
			model:          &jamiethompsonmev1alpha1.Model{Type: jamiethompsonmev1alpha1.TypeXGBoost, XGBoost: &jamiethompsonmev1alpha1.XGBoost{HistorySize: 5, LookAhead: 1000, Lags: 2}},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{},
		},
		{
			description: "Success single CPU evaluation returns last replicas",
			expected:    6,
			expectedErr: nil,
			predicter:   &xgboost.Predict{},
			model: &jamiethompsonmev1alpha1.Model{
				Type:    jamiethompsonmev1alpha1.TypeXGBoost,
				XGBoost: &jamiethompsonmev1alpha1.XGBoost{HistorySize: 5, LookAhead: 1000, Lags: 2},
			},
			replicaHistory: attachCPUUsage([]jamiethompsonmev1alpha1.TimestampedReplicas{
				{Replicas: 6},
			}),
		},
		{
			description: "Success converts predicted CPU usage to replicas",
			expected:    4,
			expectedErr: nil,
			predicter: &xgboost.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "2240", nil
					},
				},
			},
			model: &jamiethompsonmev1alpha1.Model{
				Type:                           jamiethompsonmev1alpha1.TypeXGBoost,
				CPURequestPerPodMillicores:     800,
				TargetCPUUtilizationPercentage: 70,
				XGBoost: &jamiethompsonmev1alpha1.XGBoost{
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

func TestPredict_GetPredictionResult_ConsumedUntilUsesTrainingHistory(t *testing.T) {
	p := &xgboost.Predict{
		Runner: &fake.Run{
			RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
				return "2240", nil
			},
		},
	}

	model := &jamiethompsonmev1alpha1.Model{
		Type:                           jamiethompsonmev1alpha1.TypeXGBoost,
		CPURequestPerPodMillicores:     800,
		TargetCPUUtilizationPercentage: 70,
		XGBoost: &jamiethompsonmev1alpha1.XGBoost{
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
