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

// Package prediction provides a framework for using models to make predictions based on historical evaluations
package prediction

import (
	"fmt"
	"time"

	jamiethompsonmev1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

// Result captures both the predicted replica target and the latest history timestamp
// the predictor actually consumed while producing that prediction.
type Result struct {
	Replicas      int32
	ConsumedUntil *time.Time
}

// Predicter is an interface providing methods for making a prediction based on a model, a time to predict and values
type Predicter interface {
	GetPredictionResult(model *jamiethompsonmev1alpha1.Model, replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) (Result, error)
	GetPrediction(model *jamiethompsonmev1alpha1.Model, replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) (int32, error)
	PruneHistory(model *jamiethompsonmev1alpha1.Model, replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) ([]jamiethompsonmev1alpha1.TimestampedReplicas, error)
	GetType() string
}

// ModelPredict is used to route a prediction to the appropriate predicter based on the model provided
// Should be initialised with available predicters for it to use
type ModelPredict struct {
	Predicters []Predicter
}

// GetPrediction generates a prediction for any model that the ModelPredict has been set up to use
func (m *ModelPredict) GetPrediction(model *jamiethompsonmev1alpha1.Model, replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) (int32, error) {
	result, err := m.GetPredictionResult(model, replicaHistory)
	if err != nil {
		return 0, err
	}
	return result.Replicas, nil
}

// GetPredictionResult generates a prediction result for any model that the ModelPredict has been set up to use
func (m *ModelPredict) GetPredictionResult(model *jamiethompsonmev1alpha1.Model, replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) (Result, error) {
	for _, predicter := range m.Predicters {
		if predicter.GetType() == model.Type {
			return predicter.GetPredictionResult(model, replicaHistory)
		}
	}
	return Result{}, fmt.Errorf("unknown model type '%s'", model.Type)
}

// GetIDsToRemove finds the appropriate logic for the model and gets a list of stored IDs to remove
func (m *ModelPredict) PruneHistory(model *jamiethompsonmev1alpha1.Model, replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) ([]jamiethompsonmev1alpha1.TimestampedReplicas, error) {
	for _, predicter := range m.Predicters {
		if predicter.GetType() == model.Type {
			return predicter.PruneHistory(model, replicaHistory)
		}
	}
	return nil, fmt.Errorf("unknown model type '%s'", model.Type)
}

// GetType returns the type of the ModelPredict, "Model"
func (m *ModelPredict) GetType() string {
	return "Model"
}

// LatestTimestamp returns the latest non-nil timestamp in the provided history.
func LatestTimestamp(replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) *time.Time {
	var latest *time.Time
	for _, entry := range replicaHistory {
		if entry.Time == nil {
			continue
		}
		timestamp := entry.Time.Time
		if latest == nil || timestamp.After(*latest) {
			copy := timestamp
			latest = &copy
		}
	}
	return latest
}

// UsesCPUHistory reports whether the model trains on aggregate CPU usage samples instead of replica counts.
func UsesCPUHistory(modelType string) bool {
	switch modelType {
	case jamiethompsonmev1alpha1.TypeLinear,
		jamiethompsonmev1alpha1.TypeHoltWinters,
		jamiethompsonmev1alpha1.TypeArima,
		jamiethompsonmev1alpha1.TypeXGBoost,
		jamiethompsonmev1alpha1.TypeLightGBM:
		return true
	default:
		return false
	}
}
