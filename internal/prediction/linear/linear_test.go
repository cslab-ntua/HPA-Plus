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

package linear_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	jamiethompsonmev1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/fake"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction/linear"
	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func intPtr(i int) *int {
	return &i
}

func withCPUHistory(history []jamiethompsonmev1alpha1.TimestampedReplicas) []jamiethompsonmev1alpha1.TimestampedReplicas {
	out := make([]jamiethompsonmev1alpha1.TimestampedReplicas, len(history))
	copy(out, history)
	for i := range out {
		if out[i].TotalCPUUsageMillicores == nil {
			value := int64(out[i].Replicas)
			out[i].TotalCPUUsageMillicores = &value
		}
	}
	return out
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
		predicter      *linear.Predict
		model          *jamiethompsonmev1alpha1.Model
		replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas
	}{
		{
			description:    "Fail no Linear configuration",
			expected:       0,
			expectedErr:    errors.New("no Linear configuration provided for model"),
			predicter:      &linear.Predict{},
			model:          &jamiethompsonmev1alpha1.Model{},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{},
		},
		{
			description: "Fail no evaluations",
			expected:    0,
			expectedErr: errors.New("no CPU usage evaluations provided for Linear regression model"),
			predicter:   &linear.Predict{},
			model: &jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeLinear,
				Linear: &jamiethompsonmev1alpha1.Linear{
					HistorySize: 5,
					LookAhead:   0,
				},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{},
		},
		{
			description: "Success, only one evaluation, return without the prediction",
			expected:    32,
			expectedErr: nil,
			predicter:   &linear.Predict{},
			model: &jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeLinear,
				Linear: &jamiethompsonmev1alpha1.Linear{
					HistorySize: 5,
					LookAhead:   0,
				},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 32,
				},
			},
		},
		{
			description: "Fail execution of algorithm fails",
			expected:    0,
			expectedErr: errors.New("algorithm fail"),
			predicter: &linear.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "", errors.New("algorithm fail")
					},
				},
			},
			model: &jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeLinear,
				Linear: &jamiethompsonmev1alpha1.Linear{
					HistorySize: 5,
					LookAhead:   0,
				},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
			},
		},
		{
			description: "Fail algorithm returns non-integer castable value",
			expected:    0,
			expectedErr: errors.New(`strconv.ParseInt: parsing "invalid": invalid syntax`),
			predicter: &linear.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "invalid", nil
					},
				},
			},
			model: &jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeLinear,
				Linear: &jamiethompsonmev1alpha1.Linear{
					HistorySize: 5,
					LookAhead:   0,
				},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
			},
		},
		{
			description: "Success",
			expected:    3,
			expectedErr: nil,
			predicter: &linear.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "3", nil
					},
				},
			},
			model: &jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeLinear,
				Linear: &jamiethompsonmev1alpha1.Linear{
					HistorySize: 5,
					LookAhead:   0,
				},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
			},
		},
		{
			description: "Success, use custom timeout",
			expected:    3,
			expectedErr: nil,
			predicter: &linear.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "3", nil
					},
				},
			},
			model: &jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeLinear,
				Linear: &jamiethompsonmev1alpha1.Linear{
					HistorySize: 5,
					LookAhead:   0,
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
			},
		},
	}
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			history := withCPUHistory(test.replicaHistory)
			modelCopy := *test.model
			if modelCopy.CPURequestPerPodMillicores == 0 {
				modelCopy.CPURequestPerPodMillicores = 1
			}
			if modelCopy.TargetCPUUtilizationPercentage == 0 {
				modelCopy.TargetCPUUtilizationPercentage = 100
			}

			result, err := test.predicter.GetPrediction(&modelCopy, history)
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

func TestPredict_PruneHistoryUsesCPUBackedEntries(t *testing.T) {
	predicter := &linear.Predict{}
	model := &jamiethompsonmev1alpha1.Model{
		Linear: &jamiethompsonmev1alpha1.Linear{
			HistorySize: 2,
		},
	}

	cpu1 := int64(100)
	cpu2 := int64(200)
	cpu3 := int64(300)
	history := []jamiethompsonmev1alpha1.TimestampedReplicas{
		{Replicas: 1, Time: &metav1.Time{Time: time.Unix(1, 0).UTC()}},
		{Replicas: 2, Time: &metav1.Time{Time: time.Unix(2, 0).UTC()}, TotalCPUUsageMillicores: &cpu1},
		{Replicas: 3, Time: &metav1.Time{Time: time.Unix(3, 0).UTC()}, TotalCPUUsageMillicores: &cpu2},
		{Replicas: 4, Time: &metav1.Time{Time: time.Unix(4, 0).UTC()}},
		{Replicas: 5, Time: &metav1.Time{Time: time.Unix(5, 0).UTC()}, TotalCPUUsageMillicores: &cpu3},
	}

	pruned, err := predicter.PruneHistory(model, history)
	if err != nil {
		t.Fatalf("PruneHistory() error = %v", err)
	}

	if diff := cmp.Diff(history[2:], pruned); diff != "" {
		t.Fatalf("PruneHistory() mismatch (-want +got):\n%s", diff)
	}
}

func TestPredict_PruneHistoryUsesNewestCPUEntriesWhenInputUnsorted(t *testing.T) {
	predicter := &linear.Predict{}
	model := &jamiethompsonmev1alpha1.Model{
		Linear: &jamiethompsonmev1alpha1.Linear{
			HistorySize: 2,
		},
	}

	cpu1 := int64(100)
	cpu2 := int64(200)
	cpu3 := int64(300)
	history := []jamiethompsonmev1alpha1.TimestampedReplicas{
		{Replicas: 5, Time: &metav1.Time{Time: time.Unix(5, 0).UTC()}, TotalCPUUsageMillicores: &cpu3},
		{Replicas: 2, Time: &metav1.Time{Time: time.Unix(2, 0).UTC()}, TotalCPUUsageMillicores: &cpu1},
		{Replicas: 4, Time: &metav1.Time{Time: time.Unix(4, 0).UTC()}},
		{Replicas: 3, Time: &metav1.Time{Time: time.Unix(3, 0).UTC()}, TotalCPUUsageMillicores: &cpu2},
		{Replicas: 1, Time: &metav1.Time{Time: time.Unix(1, 0).UTC()}},
	}

	pruned, err := predicter.PruneHistory(model, history)
	if err != nil {
		t.Fatalf("PruneHistory() error = %v", err)
	}

	expected := []jamiethompsonmev1alpha1.TimestampedReplicas{
		{Replicas: 3, Time: &metav1.Time{Time: time.Unix(3, 0).UTC()}, TotalCPUUsageMillicores: &cpu2},
		{Replicas: 4, Time: &metav1.Time{Time: time.Unix(4, 0).UTC()}},
		{Replicas: 5, Time: &metav1.Time{Time: time.Unix(5, 0).UTC()}, TotalCPUUsageMillicores: &cpu3},
	}
	if diff := cmp.Diff(expected, pruned); diff != "" {
		t.Fatalf("PruneHistory() mismatch (-want +got):\n%s", diff)
	}
}

func TestPredict_GetPredictionSortsTrainingHistoryBeforeRunningAlgorithm(t *testing.T) {
	type algorithmInput struct {
		ReplicaHistory []struct {
			Time string `json:"time"`
		} `json:"replicaHistory"`
	}

	model := &jamiethompsonmev1alpha1.Model{
		Type: jamiethompsonmev1alpha1.TypeLinear,
		Linear: &jamiethompsonmev1alpha1.Linear{
			HistorySize: 5,
			LookAhead:   0,
		},
		CPURequestPerPodMillicores:     100,
		TargetCPUUtilizationPercentage: 100,
	}

	history := []jamiethompsonmev1alpha1.TimestampedReplicas{
		{Replicas: 3, Time: &metav1.Time{Time: time.Unix(3, 0).UTC()}},
		{Replicas: 1, Time: &metav1.Time{Time: time.Unix(1, 0).UTC()}},
		{Replicas: 2, Time: &metav1.Time{Time: time.Unix(2, 0).UTC()}},
	}

	predicter := &linear.Predict{
		Runner: &fake.Run{
			RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
				var input algorithmInput
				if err := json.Unmarshal([]byte(value), &input); err != nil {
					t.Fatalf("json.Unmarshal() error = %v", err)
				}
				got := []string{
					input.ReplicaHistory[0].Time,
					input.ReplicaHistory[1].Time,
					input.ReplicaHistory[2].Time,
				}
				want := []string{
					"1970-01-01T00:00:01Z",
					"1970-01-01T00:00:02Z",
					"1970-01-01T00:00:03Z",
				}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Fatalf("training order mismatch (-want +got):\n%s", diff)
				}
				return "300", nil
			},
		},
	}

	result, err := predicter.GetPrediction(model, withCPUHistory(history))
	if err != nil {
		t.Fatalf("GetPrediction() error = %v", err)
	}
	if result != 3 {
		t.Fatalf("GetPrediction() = %d, want 3", result)
	}
}

func TestModelPredict_PruneHistory(t *testing.T) {
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
			description:    "Fail no Linear configuration",
			expected:       nil,
			expectedErr:    errors.New("no Linear configuration provided for model"),
			model:          &jamiethompsonmev1alpha1.Model{},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{},
		},
		{
			description: "Only 3 in history, max size 4",
			expected: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 4,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(6) * time.Second)},
				},
				{
					Replicas: 2,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(5) * time.Second)},
				},
				{
					Replicas: 1,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(4) * time.Second)},
				},
			},
			expectedErr: nil,
			model: &jamiethompsonmev1alpha1.Model{
				Linear: &jamiethompsonmev1alpha1.Linear{
					HistorySize: 4,
				},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 4,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(6) * time.Second)},
				},
				{
					Replicas: 2,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(5) * time.Second)},
				},
				{
					Replicas: 1,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(4) * time.Second)},
				},
			},
		},
		{
			description: "3 too many, remove oldest 3",
			expected: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 4,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(6) * time.Second)},
				},
				{
					Replicas: 2,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(5) * time.Second)},
				},
				{
					Replicas: 1,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(4) * time.Second)},
				},
			},
			expectedErr: nil,
			model: &jamiethompsonmev1alpha1.Model{
				Linear: &jamiethompsonmev1alpha1.Linear{
					HistorySize: 3,
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
				// START OLDEST
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
				// END OLDEST
				{
					Replicas: 4,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(6) * time.Second)},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			predicter := &linear.Predict{}
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
			expected:    "Linear",
		},
	}
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			predicter := &linear.Predict{}
			result := predicter.GetType()
			if !cmp.Equal(test.expected, result) {
				t.Errorf("type mismatch (-want +got):\n%s", cmp.Diff(test.expected, result))
			}
		})
	}
}
