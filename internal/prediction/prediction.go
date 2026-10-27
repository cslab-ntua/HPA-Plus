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
	"context"
	"errors"
	"fmt"
	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
	"time"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

// ErrInsufficientHistory means the model cannot produce a forecast yet. It must
// not contribute a zero candidate or mark any samples as consumed.
var ErrInsufficientHistory = errors.New("insufficient CPU history for prediction")

// Result captures both the predicted replica target and the latest history timestamp
// the predictor actually consumed while producing that prediction.
type Result struct {
	Adaptation string

	Verification *hpaplusv1alpha1.ForecastVerification

	Trajectory    []hpaplusv1alpha1.CPUForecastPoint
	Replicas      int32
	ConsumedUntil *time.Time
	Diagnostics   *algorithm.ForecastDiagnostics
}

// Predicter is an interface providing methods for making a prediction based on a model, a time to predict and values
type Predicter interface {
	GetPredictionResult(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) (Result, error)
	GetPrediction(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) (int32, error)
	PruneHistory(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) ([]hpaplusv1alpha1.TimestampedSample, error)
	GetType() string
}

// ModelPredict is used to route a prediction to the appropriate predicter based on the model provided
// Should be initialised with available predicters for it to use
type ModelPredict struct {
	Predicters []Predicter
}

// SyncSessions removes runtime state for models no longer present under a resource.
func (m *ModelPredict) SyncSessions(prefix string, active map[string]bool) {
	for _, predictor := range m.Predicters {
		if owner, ok := predictor.(interface{ SyncSessions(string, map[string]bool) }); ok {
			owner.SyncSessions(prefix, active)
		}
	}
}

// GetPrediction generates a prediction for any model that the ModelPredict has been set up to use
func (m *ModelPredict) GetPrediction(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) (int32, error) {
	result, err := m.GetPredictionResult(model, replicaHistory)
	if err != nil {
		return 0, err
	}
	return result.Replicas, nil
}

// GetPredictionResult generates a prediction result for any model that the ModelPredict has been set up to use
func (m *ModelPredict) GetPredictionResult(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) (Result, error) {
	return m.GetPredictionResultContext(context.Background(), model, replicaHistory)
}

func (m *ModelPredict) GetPredictionResultContext(ctx context.Context, model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) (Result, error) {
	for _, predicter := range m.Predicters {
		if predicter.GetType() == model.Type {
			return GetResult(ctx, predicter, model, replicaHistory)
		}
	}
	return Result{}, fmt.Errorf("unknown model type '%s'", model.Type)
}

// GetIDsToRemove finds the appropriate logic for the model and gets a list of stored IDs to remove
func (m *ModelPredict) PruneHistory(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) ([]hpaplusv1alpha1.TimestampedSample, error) {
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
func LatestTimestamp(replicaHistory []hpaplusv1alpha1.TimestampedSample) *time.Time {
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
	case hpaplusv1alpha1.TypeLinear,
		hpaplusv1alpha1.TypeHoltWinters,
		hpaplusv1alpha1.TypeArima,
		hpaplusv1alpha1.TypeXGBoost,
		hpaplusv1alpha1.TypeLightGBM:
		return true
	default:
		return false
	}
}

// ForecastDemand routes typed demand without performing capacity conversion.
func (m *ModelPredict) ForecastDemand(ctx context.Context, request DemandRequest) (DemandResult, error) {
	if err := ctx.Err(); err != nil {
		return DemandResult{}, err
	}
	if request.Model == nil {
		return DemandResult{}, fmt.Errorf("missing model")
	}
	for _, p := range m.Predicters {
		if p.GetType() == request.Model.Type {
			if typed, ok := p.(DemandPredicter); ok {
				return typed.ForecastDemand(ctx, request)
			}
			return DemandResult{}, fmt.Errorf("model does not implement typed forecasting")
		}
	}
	return DemandResult{}, fmt.Errorf("unknown model type")
}
