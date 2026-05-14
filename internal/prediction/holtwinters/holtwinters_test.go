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

package holtwinters_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	jamiethompsonmev1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/fake"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction/holtwinters"
	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func intPtr(i int) *int {
	return &i
}

func float64Ptr(val float64) *float64 {
	return &val
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
		predicter      *holtwinters.Predict
		model          *jamiethompsonmev1alpha1.Model
		replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas
	}{
		{
			"Fail no HoltWinters configuration",
			0,
			errors.New("no HoltWinters configuration provided for model"),
			&holtwinters.Predict{},
			&jamiethompsonmev1alpha1.Model{},
			[]jamiethompsonmev1alpha1.TimestampedReplicas{},
		},
		{
			"Fail no trend configuration",
			0,
			errors.New("no required 'trend' value provided for model"),
			&holtwinters.Predict{},
			&jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeLinear,
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					Trend: "",
				},
			},
			[]jamiethompsonmev1alpha1.TimestampedReplicas{},
		},
		{
			"Success, less than 2 * seasonal_periods observations",
			0,
			nil,
			&holtwinters.Predict{},
			&jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeLinear,
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					Alpha:           float64Ptr(0.9),
					Beta:            float64Ptr(0.9),
					Gamma:           float64Ptr(0.9),
					SeasonalPeriods: 3,
					StoredSeasons:   3,
					Trend:           "add",
				},
			},
			[]jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Time:     &metav1.Time{Time: time.Now().UTC().Add(time.Duration(-80) * time.Second)},
					Replicas: 1,
				},
				{
					Time:     &metav1.Time{Time: time.Now().UTC().Add(time.Duration(-70) * time.Second)},
					Replicas: 3,
				},
			},
		},
		{
			"Success, less than 10 + 2 * (seasonal_periods // 2) observations",
			0,
			nil,
			&holtwinters.Predict{},
			&jamiethompsonmev1alpha1.Model{
				Type: jamiethompsonmev1alpha1.TypeLinear,
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					Alpha:           float64Ptr(0.9),
					Beta:            float64Ptr(0.9),
					Gamma:           float64Ptr(0.9),
					SeasonalPeriods: 3,
					StoredSeasons:   3,
					Trend:           "add",
				},
			},
			[]jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Time:     &metav1.Time{Time: time.Now().UTC().Add(time.Duration(-80) * time.Second)},
					Replicas: 1,
				},
				{
					Time:     &metav1.Time{Time: time.Now().UTC().Add(time.Duration(-70) * time.Second)},
					Replicas: 3,
				},
				{
					Time:     &metav1.Time{Time: time.Now().UTC().Add(time.Duration(-60) * time.Second)},
					Replicas: 1,
				},
				{
					Time:     &metav1.Time{Time: time.Now().UTC().Add(time.Duration(-50) * time.Second)},
					Replicas: 1,
				},
				{
					Time:     &metav1.Time{Time: time.Now().UTC().Add(time.Duration(-40) * time.Second)},
					Replicas: 3,
				},
				{
					Time:     &metav1.Time{Time: time.Now().UTC().Add(time.Duration(-30) * time.Second)},
					Replicas: 1,
				},
				{
					Time:     &metav1.Time{Time: time.Now().UTC().Add(time.Duration(-20) * time.Second)},
					Replicas: 1,
				},
			},
		},
		{
			"Fail, fail to runtime fetch",
			0,
			errors.New("fail runtime fetch"),
			&holtwinters.Predict{
				HookExecute: func() *fake.Execute {
					execute := fake.Execute{}
					execute.ExecuteWithValueReactor = func(definition *jamiethompsonmev1alpha1.HookDefinition, value string) (string, error) {
						return "", errors.New("fail runtime fetch")
					}
					return &execute
				}(),
			},
			&jamiethompsonmev1alpha1.Model{
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					RuntimeTuningFetchHook: &jamiethompsonmev1alpha1.HookDefinition{
						Type:    "test",
						Timeout: 2500,
					},
					SeasonalPeriods: 2,
					Trend:           "add",
					Seasonal:        "add",
				},
			},
			[]jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
				{
					Replicas: 3,
				},
				{
					Replicas: 4,
				},
				{
					Replicas: 5,
				},
				{
					Replicas: 6,
				},
				{
					Replicas: 7,
				},
				{
					Replicas: 8,
				},
				{
					Replicas: 9,
				},
				{
					Replicas: 10,
				},
				{
					Replicas: 11,
				},
				{
					Replicas: 12,
				},
				{
					Replicas: 13,
				},
				{
					Replicas: 14,
				},
			},
		},
		{
			"Fail, invalid runtime fetch response",
			0,
			errors.New("invalid character 'i' looking for beginning of value"),
			&holtwinters.Predict{
				HookExecute: func() *fake.Execute {
					execute := fake.Execute{}
					execute.ExecuteWithValueReactor = func(definition *jamiethompsonmev1alpha1.HookDefinition, value string) (string, error) {
						return "invalid json", nil
					}
					return &execute
				}(),
			},
			&jamiethompsonmev1alpha1.Model{
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					RuntimeTuningFetchHook: &jamiethompsonmev1alpha1.HookDefinition{
						Type:    "test",
						Timeout: 2500,
					},
					SeasonalPeriods: 2,
					Trend:           "add",
					Seasonal:        "add",
				},
			},
			[]jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
				{
					Replicas: 3,
				},
				{
					Replicas: 4,
				},
				{
					Replicas: 5,
				},
				{
					Replicas: 6,
				},
				{
					Replicas: 7,
				},
				{
					Replicas: 8,
				},
				{
					Replicas: 9,
				},
				{
					Replicas: 10,
				},
				{
					Replicas: 11,
				},
				{
					Replicas: 12,
				},
				{
					Replicas: 13,
				},
				{
					Replicas: 14,
				},
			},
		},
		{
			"Fail no alpha value",
			0,
			errors.New("no alpha tuning value provided for Holt-Winters prediction"),
			&holtwinters.Predict{},
			&jamiethompsonmev1alpha1.Model{
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					Beta:            float64Ptr(0.9),
					Gamma:           float64Ptr(0.9),
					SeasonalPeriods: 2,
					Trend:           "add",
					Seasonal:        "add",
				},
			},
			[]jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
				{
					Replicas: 3,
				},
				{
					Replicas: 4,
				},
				{
					Replicas: 5,
				},
				{
					Replicas: 6,
				},
				{
					Replicas: 7,
				},
				{
					Replicas: 8,
				},
				{
					Replicas: 9,
				},
				{
					Replicas: 10,
				},
				{
					Replicas: 11,
				},
				{
					Replicas: 12,
				},
				{
					Replicas: 13,
				},
				{
					Replicas: 14,
				},
			},
		},
		{
			"Fail no beta value",
			0,
			errors.New("no beta tuning value provided for Holt-Winters prediction"),
			&holtwinters.Predict{},
			&jamiethompsonmev1alpha1.Model{
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					Alpha:           float64Ptr(0.9),
					Gamma:           float64Ptr(0.9),
					SeasonalPeriods: 2,
					Trend:           "add",
					Seasonal:        "add",
				},
			},
			[]jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
				{
					Replicas: 3,
				},
				{
					Replicas: 4,
				},
				{
					Replicas: 5,
				},
				{
					Replicas: 6,
				},
				{
					Replicas: 7,
				},
				{
					Replicas: 8,
				},
				{
					Replicas: 9,
				},
				{
					Replicas: 10,
				},
				{
					Replicas: 11,
				},
				{
					Replicas: 12,
				},
				{
					Replicas: 13,
				},
				{
					Replicas: 14,
				},
			},
		},
		{
			"Fail no gamma value",
			0,
			errors.New("no gamma tuning value provided for Holt-Winters prediction"),
			&holtwinters.Predict{},
			&jamiethompsonmev1alpha1.Model{
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					Alpha:           float64Ptr(0.9),
					Beta:            float64Ptr(0.9),
					SeasonalPeriods: 2,
					Trend:           "add",
					Seasonal:        "add",
				},
			},
			[]jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
				{
					Replicas: 3,
				},
				{
					Replicas: 4,
				},
				{
					Replicas: 5,
				},
				{
					Replicas: 6,
				},
				{
					Replicas: 7,
				},
				{
					Replicas: 8,
				},
				{
					Replicas: 9,
				},
				{
					Replicas: 10,
				},
				{
					Replicas: 11,
				},
				{
					Replicas: 12,
				},
				{
					Replicas: 13,
				},
				{
					Replicas: 14,
				},
			},
		},
		{
			"Fail, additive, fail to run holt winters algorithm",
			0,
			errors.New("holt winters algorithm error"),
			&holtwinters.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "", errors.New("holt winters algorithm error")
					},
				},
			},
			&jamiethompsonmev1alpha1.Model{
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					Alpha:           float64Ptr(0.9),
					Beta:            float64Ptr(0.9),
					Gamma:           float64Ptr(0.9),
					SeasonalPeriods: 2,
					Trend:           "additive",
				},
			},
			[]jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
				{
					Replicas: 3,
				},
				{
					Replicas: 4,
				},
				{
					Replicas: 5,
				},
				{
					Replicas: 6,
				},
				{
					Replicas: 7,
				},
				{
					Replicas: 8,
				},
				{
					Replicas: 9,
				},
				{
					Replicas: 10,
				},
				{
					Replicas: 11,
				},
				{
					Replicas: 12,
				},
				{
					Replicas: 13,
				},
				{
					Replicas: 14,
				},
			},
		},
		{
			"Fail, additive, holt winters algorithm invalid response",
			0,
			errors.New(`strconv.ParseInt: parsing "invalid": invalid syntax`),
			&holtwinters.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "invalid", nil
					},
				},
			},
			&jamiethompsonmev1alpha1.Model{
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					Alpha:           float64Ptr(0.9),
					Beta:            float64Ptr(0.9),
					Gamma:           float64Ptr(0.9),
					SeasonalPeriods: 2,
					Trend:           "additive",
				},
			},
			[]jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
				{
					Replicas: 3,
				},
				{
					Replicas: 4,
				},
				{
					Replicas: 5,
				},
				{
					Replicas: 6,
				},
				{
					Replicas: 7,
				},
				{
					Replicas: 8,
				},
				{
					Replicas: 9,
				},
				{
					Replicas: 10,
				},
				{
					Replicas: 11,
				},
				{
					Replicas: 12,
				},
				{
					Replicas: 13,
				},
				{
					Replicas: 14,
				},
			},
		},
		{
			"Success",
			0,
			nil,
			&holtwinters.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "0", nil
					},
				},
			},
			&jamiethompsonmev1alpha1.Model{
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					Alpha:           float64Ptr(0.9),
					Beta:            float64Ptr(0.9),
					Gamma:           float64Ptr(0.9),
					SeasonalPeriods: 2,
					Trend:           "add",
					Seasonal:        "add",
				},
			},
			[]jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
				{
					Replicas: 3,
				},
				{
					Replicas: 4,
				},
				{
					Replicas: 5,
				},
				{
					Replicas: 6,
				},
				{
					Replicas: 7,
				},
				{
					Replicas: 8,
				},
				{
					Replicas: 9,
				},
				{
					Replicas: 10,
				},
				{
					Replicas: 11,
				},
				{
					Replicas: 12,
				},
				{
					Replicas: 13,
				},
				{
					Replicas: 14,
				},
			},
		},
		{
			"Success, configure calculation timeout",
			0,
			nil,
			&holtwinters.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "0", nil
					},
				},
			},
			&jamiethompsonmev1alpha1.Model{
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					Alpha:           float64Ptr(0.9),
					Beta:            float64Ptr(0.9),
					Gamma:           float64Ptr(0.9),
					SeasonalPeriods: 2,
					Trend:           "add",
					Seasonal:        "add",
				},
				CalculationTimeout: intPtr(10),
			},
			[]jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
				{
					Replicas: 3,
				},
				{
					Replicas: 4,
				},
				{
					Replicas: 5,
				},
				{
					Replicas: 6,
				},
				{
					Replicas: 7,
				},
				{
					Replicas: 8,
				},
				{
					Replicas: 9,
				},
				{
					Replicas: 10,
				},
				{
					Replicas: 11,
				},
				{
					Replicas: 12,
				},
				{
					Replicas: 13,
				},
				{
					Replicas: 14,
				},
			},
		},
		{
			"Success, use fetch but no values returned, so use hardcoded fallback",
			0,
			nil,
			&holtwinters.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "0", nil
					},
				},
				HookExecute: func() *fake.Execute {
					execute := fake.Execute{}
					execute.ExecuteWithValueReactor = func(definition *jamiethompsonmev1alpha1.HookDefinition, value string) (string, error) {
						return `{}`, nil
					}
					return &execute
				}(),
			},
			&jamiethompsonmev1alpha1.Model{
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					RuntimeTuningFetchHook: &jamiethompsonmev1alpha1.HookDefinition{
						Type:    "test",
						Timeout: 2500,
					},
					Alpha:           float64Ptr(0.9),
					Beta:            float64Ptr(0.9),
					Gamma:           float64Ptr(0.9),
					SeasonalPeriods: 2,
					Trend:           "add",
					Seasonal:        "add",
				},
			},
			[]jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
				{
					Replicas: 3,
				},
				{
					Replicas: 4,
				},
				{
					Replicas: 5,
				},
				{
					Replicas: 6,
				},
				{
					Replicas: 7,
				},
				{
					Replicas: 8,
				},
				{
					Replicas: 9,
				},
				{
					Replicas: 10,
				},
				{
					Replicas: 11,
				},
				{
					Replicas: 12,
				},
				{
					Replicas: 13,
				},
				{
					Replicas: 14,
				},
			},
		},
		{
			"Success, provide all values from fetch",
			2,
			nil,
			&holtwinters.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "2", nil
					},
				},
				HookExecute: func() *fake.Execute {
					execute := fake.Execute{}
					execute.ExecuteWithValueReactor = func(definition *jamiethompsonmev1alpha1.HookDefinition, value string) (string, error) {
						return `{"alpha":0.2, "beta":0.2, "gamma": 0.2}`, nil
					}
					return &execute
				}(),
			},
			&jamiethompsonmev1alpha1.Model{
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					RuntimeTuningFetchHook: &jamiethompsonmev1alpha1.HookDefinition{
						Type:    "test",
						Timeout: 2500,
					},
					SeasonalPeriods: 2,
					Trend:           "add",
					Seasonal:        "add",
				},
			},
			[]jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
				{
					Replicas: 3,
				},
				{
					Replicas: 4,
				},
				{
					Replicas: 5,
				},
				{
					Replicas: 6,
				},
				{
					Replicas: 7,
				},
				{
					Replicas: 8,
				},
				{
					Replicas: 9,
				},
				{
					Replicas: 10,
				},
				{
					Replicas: 11,
				},
				{
					Replicas: 12,
				},
				{
					Replicas: 13,
				},
				{
					Replicas: 14,
				},
			},
		},
		{
			"Success, provide alpha and beta values from fetch",
			3,
			nil,
			&holtwinters.Predict{
				Runner: &fake.Run{
					RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
						return "3", nil
					},
				},
				HookExecute: func() *fake.Execute {
					execute := fake.Execute{}
					execute.ExecuteWithValueReactor = func(definition *jamiethompsonmev1alpha1.HookDefinition, value string) (string, error) {
						return `{"alpha":0.2, "beta":0.2}`, nil
					}
					return &execute
				}(),
			},
			&jamiethompsonmev1alpha1.Model{
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					RuntimeTuningFetchHook: &jamiethompsonmev1alpha1.HookDefinition{
						Type:    "test",
						Timeout: 2500,
					},
					Gamma:           float64Ptr(0.9),
					SeasonalPeriods: 2,
					Trend:           "add",
					Seasonal:        "add",
				},
			},
			[]jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
				},
				{
					Replicas: 2,
				},
				{
					Replicas: 3,
				},
				{
					Replicas: 4,
				},
				{
					Replicas: 5,
				},
				{
					Replicas: 6,
				},
				{
					Replicas: 7,
				},
				{
					Replicas: 8,
				},
				{
					Replicas: 9,
				},
				{
					Replicas: 10,
				},
				{
					Replicas: 11,
				},
				{
					Replicas: 12,
				},
				{
					Replicas: 13,
				},
				{
					Replicas: 14,
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

func TestPredict_PruneHistoryUsesCPUBackedSeasons(t *testing.T) {
	predicter := &holtwinters.Predict{}
	model := &jamiethompsonmev1alpha1.Model{
		HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
			SeasonalPeriods: 2,
			StoredSeasons:   2,
			Trend:           "add",
			Seasonal:        "add",
		},
	}

	cpu1 := int64(100)
	cpu2 := int64(200)
	cpu3 := int64(300)
	cpu4 := int64(400)
	cpu5 := int64(500)
	cpu6 := int64(600)
	history := []jamiethompsonmev1alpha1.TimestampedReplicas{
		{Replicas: 1, Time: &metav1.Time{Time: time.Unix(1, 0).UTC()}, TotalCPUUsageMillicores: &cpu1},
		{Replicas: 2, Time: &metav1.Time{Time: time.Unix(2, 0).UTC()}, TotalCPUUsageMillicores: &cpu1},
		{Replicas: 3, Time: &metav1.Time{Time: time.Unix(3, 0).UTC()}, TotalCPUUsageMillicores: &cpu2},
		{Replicas: 4, Time: &metav1.Time{Time: time.Unix(4, 0).UTC()}},
		{Replicas: 5, Time: &metav1.Time{Time: time.Unix(5, 0).UTC()}, TotalCPUUsageMillicores: &cpu3},
		{Replicas: 6, Time: &metav1.Time{Time: time.Unix(6, 0).UTC()}, TotalCPUUsageMillicores: &cpu4},
		{Replicas: 7, Time: &metav1.Time{Time: time.Unix(7, 0).UTC()}, TotalCPUUsageMillicores: &cpu5},
		{Replicas: 8, Time: &metav1.Time{Time: time.Unix(8, 0).UTC()}, TotalCPUUsageMillicores: &cpu6},
	}

	pruned, err := predicter.PruneHistory(model, history)
	if err != nil {
		t.Fatalf("PruneHistory() error = %v", err)
	}

	if diff := cmp.Diff(history[4:], pruned); diff != "" {
		t.Fatalf("PruneHistory() mismatch (-want +got):\n%s", diff)
	}
}

func TestPredict_GetPredictionSortsTrainingHistoryBeforeRunningAlgorithm(t *testing.T) {
	type algorithmInput struct {
		Series []float64 `json:"series"`
	}

	model := &jamiethompsonmev1alpha1.Model{
		HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
			Alpha:           float64Ptr(0.9),
			Beta:            float64Ptr(0.9),
			Gamma:           float64Ptr(0.9),
			SeasonalPeriods: 2,
			StoredSeasons:   6,
			Trend:           "add",
			Seasonal:        "add",
		},
		CPURequestPerPodMillicores:     100,
		TargetCPUUtilizationPercentage: 100,
	}

	history := make([]jamiethompsonmev1alpha1.TimestampedReplicas, 0, 12)
	for i := 12; i >= 1; i-- {
		replicas := int32(i)
		usage := int64(i * 100)
		history = append(history, jamiethompsonmev1alpha1.TimestampedReplicas{
			Replicas:                replicas,
			Time:                    &metav1.Time{Time: time.Unix(int64(i), 0).UTC()},
			TotalCPUUsageMillicores: &usage,
		})
	}

	predicter := &holtwinters.Predict{
		Runner: &fake.Run{
			RunAlgorithmWithValueReactor: func(algorithmPath, value string, timeout int) (string, error) {
				var input algorithmInput
				if err := json.Unmarshal([]byte(value), &input); err != nil {
					t.Fatalf("json.Unmarshal() error = %v", err)
				}
				want := []float64{100, 200, 300, 400, 500, 600, 700, 800, 900, 1000, 1100, 1200}
				if diff := cmp.Diff(want, input.Series); diff != "" {
					t.Fatalf("series order mismatch (-want +got):\n%s", diff)
				}
				return "300", nil
			},
		},
	}

	result, err := predicter.GetPrediction(model, history)
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
			description:    "Fail no HoltWinters configuration",
			expected:       nil,
			expectedErr:    errors.New("no HoltWinters configuration provided for model"),
			model:          &jamiethompsonmev1alpha1.Model{},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{},
		},
		{
			description: "Fail no trend configuration",
			expected:    nil,
			expectedErr: errors.New("no required 'trend' value provided for model"),
			model: &jamiethompsonmev1alpha1.Model{
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{},
		},
		{
			description: "6 in history, seasonal period 2, 3 stored seasons, don't prune",
			expected: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(1) * time.Second)},
				},
				{
					Replicas: 2,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(2) * time.Second)},
				},
				{
					Replicas: 3,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(3) * time.Second)},
				},
				{
					Replicas: 4,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(4) * time.Second)},
				},
				{
					Replicas: 5,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(5) * time.Second)},
				},
				{
					Replicas: 6,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(6) * time.Second)},
				},
			},
			expectedErr: nil,
			model: &jamiethompsonmev1alpha1.Model{
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					Trend:           "add",
					StoredSeasons:   3,
					SeasonalPeriods: 2,
				},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 6,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(6) * time.Second)},
				},
				{
					Replicas: 5,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(5) * time.Second)},
				},
				{
					Replicas: 4,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(4) * time.Second)},
				},
				{
					Replicas: 3,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(3) * time.Second)},
				},
				{
					Replicas: 2,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(2) * time.Second)},
				},
				{
					Replicas: 1,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(1) * time.Second)},
				},
			},
		},
		{
			description: "7 in history, seasonal period 2, 3 stored seasons, don't prune",
			expected: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 1,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(1) * time.Second)},
				},
				{
					Replicas: 2,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(2) * time.Second)},
				},
				{
					Replicas: 3,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(3) * time.Second)},
				},
				{
					Replicas: 4,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(4) * time.Second)},
				},
				{
					Replicas: 5,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(5) * time.Second)},
				},
				{
					Replicas: 6,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(6) * time.Second)},
				},
				{
					Replicas: 7,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(7) * time.Second)},
				},
			},
			expectedErr: nil,
			model: &jamiethompsonmev1alpha1.Model{
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					Trend:           "add",
					StoredSeasons:   3,
					SeasonalPeriods: 2,
				},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 7,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(7) * time.Second)},
				},
				{
					Replicas: 6,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(6) * time.Second)},
				},
				{
					Replicas: 5,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(5) * time.Second)},
				},
				{
					Replicas: 4,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(4) * time.Second)},
				},
				{
					Replicas: 3,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(3) * time.Second)},
				},
				{
					Replicas: 2,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(2) * time.Second)},
				},
				{
					Replicas: 1,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(1) * time.Second)},
				},
			},
		},
		{
			description: "8 in history, seasonal period 2, 3 stored seasons, prune oldest season",
			expected: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 3,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(3) * time.Second)},
				},
				{
					Replicas: 4,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(4) * time.Second)},
				},
				{
					Replicas: 5,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(5) * time.Second)},
				},
				{
					Replicas: 6,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(6) * time.Second)},
				},
				{
					Replicas: 7,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(7) * time.Second)},
				},
				{
					Replicas: 8,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(8) * time.Second)},
				},
			},
			expectedErr: nil,
			model: &jamiethompsonmev1alpha1.Model{
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					Trend:           "add",
					StoredSeasons:   3,
					SeasonalPeriods: 2,
				},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 8,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(8) * time.Second)},
				},
				{
					Replicas: 7,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(7) * time.Second)},
				},
				{
					Replicas: 6,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(6) * time.Second)},
				},
				{
					Replicas: 5,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(5) * time.Second)},
				},
				{
					Replicas: 4,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(4) * time.Second)},
				},
				{
					Replicas: 3,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(3) * time.Second)},
				},
				{
					Replicas: 2,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(2) * time.Second)},
				},
				{
					Replicas: 1,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(1) * time.Second)},
				},
			},
		},
		{
			description: "8 in history, unsorted, seasonal period 2, 3 stored seasons, prune oldest season",
			expected: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 3,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(3) * time.Second)},
				},
				{
					Replicas: 4,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(4) * time.Second)},
				},
				{
					Replicas: 5,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(5) * time.Second)},
				},
				{
					Replicas: 6,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(6) * time.Second)},
				},
				{
					Replicas: 7,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(7) * time.Second)},
				},
				{
					Replicas: 8,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(8) * time.Second)},
				},
			},
			expectedErr: nil,
			model: &jamiethompsonmev1alpha1.Model{
				HoltWinters: &jamiethompsonmev1alpha1.HoltWinters{
					Trend:           "add",
					StoredSeasons:   3,
					SeasonalPeriods: 2,
				},
			},
			replicaHistory: []jamiethompsonmev1alpha1.TimestampedReplicas{
				{
					Replicas: 6,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(6) * time.Second)},
				},
				{
					Replicas: 1,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(1) * time.Second)},
				},
				{
					Replicas: 7,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(7) * time.Second)},
				},
				{
					Replicas: 2,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(2) * time.Second)},
				},
				{
					Replicas: 5,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(5) * time.Second)},
				},
				{
					Replicas: 4,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(4) * time.Second)},
				},
				{
					Replicas: 3,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(3) * time.Second)},
				},
				{
					Replicas: 8,
					Time:     &metav1.Time{Time: time.Time{}.Add(time.Duration(8) * time.Second)},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			predicter := &holtwinters.Predict{}
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
			"Successful get type",
			"HoltWinters",
		},
	}
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			predicter := holtwinters.Predict{}
			result := predicter.GetType()
			if !cmp.Equal(test.expected, result) {
				t.Errorf("type mismatch (-want +got):\n%s", cmp.Diff(test.expected, result))
			}
		})
	}
}
