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
	"encoding/json"
	"errors"
	"testing"
	"time"

	jamiethompsonmev1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/fake"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction/arima"
	"github.com/google/go-cmp/cmp"
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

type fakeIncrementalRunner struct {
	RunSessionWithValueReactor func(sessionID, algorithmPath, value string, timeout int) (string, error)
	ResetSessionReactor        func(sessionID string) error
}

func (f *fakeIncrementalRunner) RunSessionWithValue(sessionID, algorithmPath, value string, timeout int) (string, error) {
	return f.RunSessionWithValueReactor(sessionID, algorithmPath, value, timeout)
}

func (f *fakeIncrementalRunner) ResetSession(sessionID string) error {
	if f.ResetSessionReactor == nil {
		return nil
	}
	return f.ResetSessionReactor(sessionID)
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
			expectedErr: errors.New("no CPU usage evaluations provided for ARIMA model"),
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
			expectedErr: errors.New(`strconv.ParseInt: parsing "invalid": invalid syntax`),
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
			if test.model != nil && test.model.Arima != nil {
				test.model.CPURequestPerPodMillicores = 1
				test.model.TargetCPUUtilizationPercentage = 100
			}
			result, err := test.predicter.GetPrediction(test.model, attachCPUUsage(test.replicaHistory))
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
		{
			description: "Prune by CPU-valid history count",
			expected: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas:                3,
					Time:                    &metav1.Time{Time: time.Time{}.Add(3 * time.Second)},
					TotalCPUUsageMillicores: int64Ptr(3),
				},
				{
					Replicas: 4,
					Time:     &metav1.Time{Time: time.Time{}.Add(4 * time.Second)},
				},
				{
					Replicas:                5,
					Time:                    &metav1.Time{Time: time.Time{}.Add(5 * time.Second)},
					TotalCPUUsageMillicores: int64Ptr(5),
				},
				{
					Replicas:                6,
					Time:                    &metav1.Time{Time: time.Time{}.Add(6 * time.Second)},
					TotalCPUUsageMillicores: int64Ptr(6),
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
					Replicas:                1,
					Time:                    &metav1.Time{Time: time.Time{}.Add(1 * time.Second)},
					TotalCPUUsageMillicores: int64Ptr(1),
				},
				{
					Replicas: 2,
					Time:     &metav1.Time{Time: time.Time{}.Add(2 * time.Second)},
				},
				{
					Replicas:                3,
					Time:                    &metav1.Time{Time: time.Time{}.Add(3 * time.Second)},
					TotalCPUUsageMillicores: int64Ptr(3),
				},
				{
					Replicas: 4,
					Time:     &metav1.Time{Time: time.Time{}.Add(4 * time.Second)},
				},
				{
					Replicas:                5,
					Time:                    &metav1.Time{Time: time.Time{}.Add(5 * time.Second)},
					TotalCPUUsageMillicores: int64Ptr(5),
				},
				{
					Replicas:                6,
					Time:                    &metav1.Time{Time: time.Time{}.Add(6 * time.Second)},
					TotalCPUUsageMillicores: int64Ptr(6),
				},
			},
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

func TestPredict_GetPrediction_IncrementalSuccess(t *testing.T) {
	incrementalEnabled := true
	calledOneShot := false
	p := &arima.Predict{
		Runner: &fake.Run{
			RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
				calledOneShot = true
				return "0", nil
			},
		},
		IncrementalRunner: &fakeIncrementalRunner{
			RunSessionWithValueReactor: func(sessionID, algorithmPath, value string, timeout int) (string, error) {
				response, err := json.Marshal(map[string]interface{}{
					"ok":         true,
					"prediction": 7,
				})
				if err != nil {
					t.Fatal(err)
				}
				return string(response), nil
			},
		},
	}

	model := &jamiethompsonmev1alpha1.Model{
		Name:                           "default/test-scaler/traffic-predictor",
		Type:                           jamiethompsonmev1alpha1.TypeArima,
		CPURequestPerPodMillicores:     1,
		TargetCPUUtilizationPercentage: 100,
		Arima: &jamiethompsonmev1alpha1.Arima{
			Order:              []int{1, 0, 0},
			LookAhead:          60000,
			IncrementalUpdates: &incrementalEnabled,
		},
	}

	replicaHistory := []jamiethompsonmev1alpha1.TimestampedReplicas{
		{
			Replicas: 1,
			Time:     &metav1.Time{Time: time.Time{}.Add(1 * time.Second)},
		},
		{
			Replicas: 2,
			Time:     &metav1.Time{Time: time.Time{}.Add(2 * time.Second)},
		},
		{
			Replicas: 3,
			Time:     &metav1.Time{Time: time.Time{}.Add(3 * time.Second)},
		},
	}

	prediction, err := p.GetPrediction(model, attachCPUUsage(replicaHistory))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prediction != 7 {
		t.Fatalf("expected prediction 7, got %d", prediction)
	}
	if calledOneShot {
		t.Fatalf("expected incremental path to be used without one-shot fallback")
	}
}

func TestPredict_GetPrediction_IncrementalFallbackToOneShot(t *testing.T) {
	incrementalEnabled := true
	calledOneShot := false
	p := &arima.Predict{
		Runner: &fake.Run{
			RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
				calledOneShot = true
				return "5", nil
			},
		},
		IncrementalRunner: &fakeIncrementalRunner{
			RunSessionWithValueReactor: func(sessionID, algorithmPath, value string, timeout int) (string, error) {
				return "", errors.New("worker unavailable")
			},
		},
	}

	model := &jamiethompsonmev1alpha1.Model{
		Name:                           "default/test-scaler/traffic-predictor",
		Type:                           jamiethompsonmev1alpha1.TypeArima,
		CPURequestPerPodMillicores:     1,
		TargetCPUUtilizationPercentage: 100,
		Arima: &jamiethompsonmev1alpha1.Arima{
			Order:              []int{1, 0, 0},
			LookAhead:          60000,
			IncrementalUpdates: &incrementalEnabled,
		},
	}

	replicaHistory := []jamiethompsonmev1alpha1.TimestampedReplicas{
		{
			Replicas: 1,
			Time:     &metav1.Time{Time: time.Time{}.Add(1 * time.Second)},
		},
		{
			Replicas: 2,
			Time:     &metav1.Time{Time: time.Time{}.Add(2 * time.Second)},
		},
		{
			Replicas: 3,
			Time:     &metav1.Time{Time: time.Time{}.Add(3 * time.Second)},
		},
	}

	prediction, err := p.GetPrediction(model, attachCPUUsage(replicaHistory))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prediction != 5 {
		t.Fatalf("expected fallback one-shot prediction 5, got %d", prediction)
	}
	if !calledOneShot {
		t.Fatalf("expected one-shot fallback to run")
	}
}

func TestPredict_GetPrediction_IncrementalUsesSessionID(t *testing.T) {
	incrementalEnabled := true
	capturedSessionID := ""
	p := &arima.Predict{
		Runner: &fake.Run{
			RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
				return "0", nil
			},
		},
		IncrementalRunner: &fakeIncrementalRunner{
			RunSessionWithValueReactor: func(sessionID, algorithmPath, value string, timeout int) (string, error) {
				capturedSessionID = sessionID
				response, err := json.Marshal(map[string]interface{}{
					"ok":         true,
					"prediction": 9,
				})
				if err != nil {
					t.Fatal(err)
				}
				return string(response), nil
			},
		},
	}

	model := &jamiethompsonmev1alpha1.Model{
		Name:                           "traffic-predictor",
		SessionID:                      "default/test-scaler/traffic-predictor",
		Type:                           jamiethompsonmev1alpha1.TypeArima,
		CPURequestPerPodMillicores:     1,
		TargetCPUUtilizationPercentage: 100,
		Arima: &jamiethompsonmev1alpha1.Arima{
			Order:              []int{1, 0, 0},
			LookAhead:          60000,
			IncrementalUpdates: &incrementalEnabled,
		},
	}

	replicaHistory := []jamiethompsonmev1alpha1.TimestampedReplicas{
		{
			Replicas: 1,
			Time:     &metav1.Time{Time: time.Time{}.Add(1 * time.Second)},
		},
		{
			Replicas: 2,
			Time:     &metav1.Time{Time: time.Time{}.Add(2 * time.Second)},
		},
		{
			Replicas: 3,
			Time:     &metav1.Time{Time: time.Time{}.Add(3 * time.Second)},
		},
	}

	prediction, err := p.GetPrediction(model, attachCPUUsage(replicaHistory))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prediction != 9 {
		t.Fatalf("expected prediction 9, got %d", prediction)
	}
	if capturedSessionID != model.SessionID {
		t.Fatalf("expected session ID %q, got %q", model.SessionID, capturedSessionID)
	}
}

func TestPredict_GetPrediction_IncrementalMissingTimestampFallsBack(t *testing.T) {
	incrementalEnabled := true
	calledOneShot := false
	calledIncremental := false

	p := &arima.Predict{
		Runner: &fake.Run{
			RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
				calledOneShot = true
				return "4", nil
			},
		},
		IncrementalRunner: &fakeIncrementalRunner{
			RunSessionWithValueReactor: func(sessionID, algorithmPath, value string, timeout int) (string, error) {
				calledIncremental = true
				return "", nil
			},
		},
	}

	model := &jamiethompsonmev1alpha1.Model{
		Name:                           "traffic-predictor",
		SessionID:                      "default/test-scaler/traffic-predictor",
		Type:                           jamiethompsonmev1alpha1.TypeArima,
		CPURequestPerPodMillicores:     1,
		TargetCPUUtilizationPercentage: 100,
		Arima: &jamiethompsonmev1alpha1.Arima{
			Order:              []int{1, 0, 0},
			LookAhead:          60000,
			IncrementalUpdates: &incrementalEnabled,
		},
	}

	replicaHistory := []jamiethompsonmev1alpha1.TimestampedReplicas{
		{
			Replicas: 1,
			Time:     &metav1.Time{Time: time.Time{}.Add(1 * time.Second)},
		},
		{
			Replicas: 2,
			Time:     nil,
		},
		{
			Replicas: 3,
			Time:     &metav1.Time{Time: time.Time{}.Add(3 * time.Second)},
		},
	}

	prediction, err := p.GetPrediction(model, attachCPUUsage(replicaHistory))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prediction != 4 {
		t.Fatalf("expected fallback prediction 4, got %d", prediction)
	}
	if !calledOneShot {
		t.Fatalf("expected one-shot path to be used")
	}
	if calledIncremental {
		t.Fatalf("did not expect incremental worker call when timestamps are missing")
	}
}

func TestPredict_GetPrediction_ConvertsPredictedCPUUsageToReplicas(t *testing.T) {
	p := &arima.Predict{
		Runner: &fake.Run{
			RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
				return "2240", nil
			},
		},
	}

	model := &jamiethompsonmev1alpha1.Model{
		Type:                           jamiethompsonmev1alpha1.TypeArima,
		CPURequestPerPodMillicores:     800,
		TargetCPUUtilizationPercentage: 70,
		Arima: &jamiethompsonmev1alpha1.Arima{
			Order:     []int{1, 0, 0},
			LookAhead: 60000,
		},
	}

	replicaHistory := attachCPUUsage([]jamiethompsonmev1alpha1.TimestampedReplicas{
		{Replicas: 2, Time: &metav1.Time{Time: time.Time{}.Add(1 * time.Second)}},
		{Replicas: 3, Time: &metav1.Time{Time: time.Time{}.Add(2 * time.Second)}},
		{Replicas: 4, Time: &metav1.Time{Time: time.Time{}.Add(3 * time.Second)}},
	})

	prediction, err := p.GetPrediction(model, replicaHistory)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prediction != 4 {
		t.Fatalf("expected prediction 4, got %d", prediction)
	}
}

func TestPredict_GetPrediction_ParsesFloatCPUUsageOutput(t *testing.T) {
	p := &arima.Predict{
		Runner: &fake.Run{
			RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
				return "2240.0", nil
			},
		},
	}

	model := &jamiethompsonmev1alpha1.Model{
		Type:                           jamiethompsonmev1alpha1.TypeArima,
		CPURequestPerPodMillicores:     800,
		TargetCPUUtilizationPercentage: 70,
		Arima: &jamiethompsonmev1alpha1.Arima{
			Order:     []int{1, 0, 0},
			LookAhead: 60000,
		},
	}

	replicaHistory := attachCPUUsage([]jamiethompsonmev1alpha1.TimestampedReplicas{
		{Replicas: 2, Time: &metav1.Time{Time: time.Time{}.Add(1 * time.Second)}},
		{Replicas: 3, Time: &metav1.Time{Time: time.Time{}.Add(2 * time.Second)}},
		{Replicas: 4, Time: &metav1.Time{Time: time.Time{}.Add(3 * time.Second)}},
	})

	prediction, err := p.GetPrediction(model, replicaHistory)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prediction != 4 {
		t.Fatalf("expected prediction 4, got %d", prediction)
	}
}

func TestPredict_GetPredictionResult_ConsumedUntilUsesTrainingHistory(t *testing.T) {
	p := &arima.Predict{
		Runner: &fake.Run{
			RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
				return "2240", nil
			},
		},
	}

	model := &jamiethompsonmev1alpha1.Model{
		Type:                           jamiethompsonmev1alpha1.TypeArima,
		CPURequestPerPodMillicores:     800,
		TargetCPUUtilizationPercentage: 70,
		Arima: &jamiethompsonmev1alpha1.Arima{
			Order:     []int{1, 0, 0},
			LookAhead: 60000,
		},
	}

	first := metav1.Time{Time: time.Time{}.Add(1 * time.Second)}
	second := metav1.Time{Time: time.Time{}.Add(2 * time.Second)}
	third := metav1.Time{Time: time.Time{}.Add(3 * time.Second)}
	fourth := metav1.Time{Time: time.Time{}.Add(4 * time.Second)}
	replicaHistory := []jamiethompsonmev1alpha1.TimestampedReplicas{
		{Replicas: 2, Time: &first, TotalCPUUsageMillicores: int64Ptr(1120)},
		{Replicas: 3, Time: &second, TotalCPUUsageMillicores: int64Ptr(1680)},
		{Replicas: 4, Time: &third, TotalCPUUsageMillicores: int64Ptr(2240)},
		{Replicas: 5, Time: &fourth},
	}

	result, err := p.GetPredictionResult(model, replicaHistory)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Replicas != 4 {
		t.Fatalf("expected prediction 4, got %d", result.Replicas)
	}
	if diff := cmp.Diff(prediction.LatestTimestamp(replicaHistory[:3]), result.ConsumedUntil); diff != "" {
		t.Fatalf("unexpected consumed timestamp (-want +got):\n%s", diff)
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
