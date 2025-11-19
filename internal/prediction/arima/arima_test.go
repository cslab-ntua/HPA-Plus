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

package arima_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	jamiethompsonmev1alpha1 "github.com/jthomperoo/predictive-horizontal-pod-autoscaler/api/v1alpha1"
	"github.com/jthomperoo/predictive-horizontal-pod-autoscaler/internal/fake"
	"github.com/jthomperoo/predictive-horizontal-pod-autoscaler/internal/prediction/arima"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func intPtr(i int) *int {
	return &i
}

func boolPtr(b bool) *bool {
	return &b
}

func stringPtr(s string) *string {
	return &s
}

func TestPredict_GetPrediction(t *testing.T) {
	equateErrorMessage := cmp.Comparer(func(x, y error) bool {
		if x == nil || y == nil {
			return x == nil && y == nil
		}
		return x.Error() == y.Error()
	})

	var tests = []struct {
		description    string
		expected       int32
		expectedErr    error
		predicter      *arima.Predict
		model          *jamiethompsonmev1alpha1.Model
		replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas
	}{
		{
			description:    "Fail no ARIMA configuration",
			expected:       0,
			expectedErr:    errors.New("no ARIMA configuration provided for model"),
			predicter:      &arima.Predict{},
			model:          &jamiethompsonmev1alpha1.Model{},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{},
		},
		{
			description: "Fail no evaluations",
			expected:    0,
			expectedErr: errors.New("no evaluations provided for ARIMA model"),
			predicter:   &arima.Predict{},
			model: &jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeArima,
				Arima: &jamiethompsonmev1alpha1.Arima{
					Order:     []int{1, 1, 1},
					LookAhead: 10000,
				},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{},
		},
		{
			description: "Success, only one evaluation, return last value",
			expected:    32,
			expectedErr: nil,
			predicter:   &arima.Predict{},
			model: &jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeArima,
				Arima: &jamiethompsonmev1alpha1.Arima{
					Order:     []int{1, 1, 1},
					LookAhead: 10000,
				},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 32,
				},
			},
		},
		{
			description: "Success, two evaluations, return last value",
			expected:    16,
			expectedErr: nil,
			predicter:   &arima.Predict{},
			model: &jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeArima,
				Arima: &jamiethompsonmev1alpha1.Arima{
					Order:     []int{1, 1, 1},
					LookAhead: 10000,
				},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 32,
				},
				{
					Replicas: 16,
				},
			},
		},
		{
			description: "Fail execution of algorithm fails",
			expected:    0,
			expectedErr: errors.New("algorithm fail"),
			predicter: &arima.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "", errors.New("algorithm fail")
					},
				},
			},
			model: &jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeArima,
				Arima: &jamiethompsonmev1alpha1.Arima{
					Order:     []int{1, 1, 1},
					LookAhead: 10000,
				},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
				{
					Replicas: 3,
				},
			},
		},
		{
			description: "Fail algorithm returns non-integer castable value",
			expected:    0,
			expectedErr: errors.New(`strconv.Atoi: parsing "invalid": invalid syntax`),
			predicter: &arima.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "invalid", nil
					},
				},
			},
			model: &jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeArima,
				Arima: &jamiethompsonmev1alpha1.Arima{
					Order:     []int{1, 1, 1},
					LookAhead: 10000,
				},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
				{
					Replicas: 3,
				},
			},
		},
		{
			description: "Success basic ARIMA",
			expected:    5,
			expectedErr: nil,
			predicter: &arima.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "5", nil
					},
				},
			},
			model: &jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeArima,
				Arima: &jamiethompsonmev1alpha1.Arima{
					Order:     []int{1, 1, 1},
					LookAhead: 10000,
				},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
				{
					Replicas: 3,
				},
			},
		},
		{
			description: "Success with auto ARIMA",
			expected:    6,
			expectedErr: nil,
			predicter: &arima.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "6", nil
					},
				},
			},
			model: &jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeArima,
				Arima: &jamiethompsonmev1alpha1.Arima{
					Order:                []int{1, 1, 1},
					LookAhead:            10000,
					AutoArima:            boolPtr(true),
					InformationCriterion: stringPtr("aic"),
					MaxOrder:             []int{3, 2, 3},
				},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
				{
					Replicas: 3,
				},
			},
		},
		{
			description: "Success, use custom timeout",
			expected:    4,
			expectedErr: nil,
			predicter: &arima.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "4", nil
					},
				},
			},
			model: &jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeArima,
				Arima: &jamiethompsonmev1alpha1.Arima{
					Order:     []int{1, 1, 1},
					LookAhead: 10000,
				},
				CalculationTimeout: intPtr(10),
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
				{
					Replicas: 3,
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			result, err := test.predicter.GetPrediction(test.model, test.replicaHistory)
			if !cmp.Equal(&err, &test.expectedErr, equateErrorMessage) {
				t.Errorf("error mismatch (-want +got):\n%s", cmp.Diff(test.expectedErr, err, equateErrorMessage))
				return
			}
			if !cmp.Equal(test.expected, result) {
				t.Errorf("result mismatch (-want +got):\n%s", cmp.Diff(test.expected, result))
			}
		})
	}
}

func TestPredict_PruneHistory(t *testing.T) {
	equateErrorMessage := cmp.Comparer(func(x, y error) bool {
		if x == nil || y == nil {
			return x == nil && y == nil
		}
		return x.Error() == y.Error()
	})

	var tests = []struct {
		description    string
		expected       []jamiethompsonmev1alpha1.TimestampedReplicas
		expectedErr    error
		model          *jamiethompsonmev1alpha1.Model
		replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas
	}{
		{
			description:    "Fail no ARIMA configuration",
			expected:       nil,
			expectedErr:    errors.New("no ARIMA configuration provided for model"),
			model:          &jamiethompsonmev1alpha1.Model{},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{},
		},
		{
			description: "Only 3 in history, max size 4",
			expected: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(4) * time.Second)},
				},
				{
					Replicas: 2,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(5) * time.Second)},
				},
				{
					Replicas: 4,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(6) * time.Second)},
				},
			},
			expectedErr: nil,
			model: &jamiethompsonmev1alpha1.Model{
				Arima: &jamiethompsonmev1alpha1.Arima{
					HistorySize: intPtr(4),
				},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(4) * time.Second)},
				},
				{
					Replicas: 2,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(5) * time.Second)},
				},
				{
					Replicas: 4,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(6) * time.Second)},
				},
			},
		},
		{
			description: "3 too many, remove oldest 3",
			expected: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(4) * time.Second)},
				},
				{
					Replicas: 2,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(5) * time.Second)},
				},
				{
					Replicas: 4,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(6) * time.Second)},
				},
			},
			expectedErr: nil,
			model: &jamiethompsonmev1alpha1.Model{
				Arima: &jamiethompsonmev1alpha1.Arima{
					HistorySize: intPtr(3),
				},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 5,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(1) * time.Second)},
				},
				{
					Replicas: 3,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(2) * time.Second)},
				},
				{
					Replicas: 8,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(3) * time.Second)},
				},
				{
					Replicas: 1,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(4) * time.Second)},
				},
				{
					Replicas: 2,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(5) * time.Second)},
				},
				{
					Replicas: 4,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(6) * time.Second)},
				},
			},
		},
		{
			description: "Default history size (50) with 60 items",
			expected: func() []jamiethompsonmev1alpha1.TimestampedReplicas {
				result := make([]jamiethompsonmev1alpha1.TimestampedReplicas, 50)
				for i := 0; i < 50; i++ {
					value := i + 11
					result[i] = jamiethompsonmev1alpha1.TimestampedReplicas{
						Replicas: int32(value),
						Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(value) * time.Second)},
					}
				}
				return result
			}(),
			expectedErr: nil,
			model:       &jamiethompsonmev1alpha1.Model{Arima: &jamiethompsonmev1alpha1.Arima{}}, // No historySize set, should use default 50
			replicaHistory: func() []jamiethompsonmev1alpha1.TimestampedReplicas {
				result := make([]jamiethompsonmev1alpha1.TimestampedReplicas, 60)
				for i := 0; i < 60; i++ {
					value := i + 1
					result[i] = jamiethompsonmev1alpha1.TimestampedReplicas{
						Replicas: int32(value),
						Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(value) * time.Second)},
					}
				}
				return result
			}(),
		},
	}
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			predicter := &arima.Predict{}
			result, err := predicter.PruneHistory(test.model, test.replicaHistory)
			if !cmp.Equal(&err, &test.expectedErr, equateErrorMessage) {
				t.Errorf("error mismatch (-want +got):\n%s", cmp.Diff(test.expectedErr, err, equateErrorMessage))
				return
			}
			if !cmp.Equal(test.expected, result) {
				t.Errorf("remove IDs mismatch (-want +got):\n%s", cmp.Diff(test.expected, result))
			}
		})
	}
}

func TestPredict_GetType(t *testing.T) {
	var tests = []struct {
		description string
		expected    string
	}{
		{
			description: "Successful get type",
			expected:    "ARIMA",
		},
	}
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			predicter := &arima.Predict{}
			result := predicter.GetType()
			if !cmp.Equal(test.expected, result) {
				t.Errorf("type mismatch (-want +got):\n%s", cmp.Diff(test.expected, result))
			}
		})
	}
}
