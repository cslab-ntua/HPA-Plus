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

package linear

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	defaultTimeout = hpaplusv1alpha1.DefaultCalculationTimeoutMillis
)

const algorithmPath = "algorithms/linear_regression/linear_regression.py"

type linearRegressionParameters struct {
	ReturnTrajectory bool                                `json:"returnTrajectory,omitempty"`
	LookAhead        int                                 `json:"lookAhead"`
	ReplicaHistory   []hpaplusv1alpha1.TimestampedSample `json:"replicaHistory"`
	CurrentTime      *metav1.Time                        `json:"currentTime"`
}

// Config represents a linear regression prediction model configuration
type Config struct {
	StoredValues int `yaml:"storedValues"`
	LookAhead    int `yaml:"lookAhead"`
}

// Runner defines an algorithm runner, allowing algorithms to be run
type AlgorithmRunner interface {
	RunAlgorithmWithValue(algorithmPath string, value string, timeout int) (string, error)
}

// Predict provides logic for using Linear Regression to make a prediction
type Predict struct {
	Runner AlgorithmRunner
}

// GetPrediction uses linear regression to predict aggregate CPU usage and converts that prediction
// back into a replica count.
func (p *Predict) GetPrediction(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) (int32, error) {
	result, err := p.GetPredictionResult(model, replicaHistory)
	if err != nil {
		return 0, err
	}
	return result.Replicas, nil
}

// GetPredictionResult uses linear regression to predict aggregate CPU usage and converts that
// prediction back into a replica count.
func (p *Predict) GetPredictionResult(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) (prediction.Result, error) {
	return p.GetPredictionResultContext(context.Background(), model, replicaHistory)
}

func (p *Predict) GetPredictionResultContext(ctx context.Context, model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) (prediction.Result, error) {
	return prediction.PredictCPU(ctx, p, model, replicaHistory)
}

func (p *Predict) AcceptedObservations() []prediction.MetricKind {
	return []prediction.MetricKind{prediction.CPUKind()}
}

func (p *Predict) ForecastDemand(ctx context.Context, request prediction.DemandRequest) (prediction.DemandResult, error) {
	if err := prediction.ValidateDemandRequest(request, p.AcceptedObservations()); err != nil {
		return prediction.DemandResult{}, err
	}
	model, replicaHistory := request.Model, request.History

	ctx, cancel, contextErr := prediction.ModelContext(ctx, model, defaultTimeout)
	if contextErr != nil {
		return prediction.DemandResult{}, contextErr
	}
	defer cancel()
	if err := ctx.Err(); err != nil {
		return prediction.DemandResult{}, err
	}
	if model.Linear == nil {
		return prediction.DemandResult{}, errors.New("no Linear configuration provided for model")
	}

	trainingHistory := prediction.FilterCPUHistory(replicaHistory)
	if len(trainingHistory) < 2 {
		return prediction.DemandResult{}, prediction.ErrInsufficientHistory
	}
	for _, entry := range trainingHistory {
		if entry.Time == nil || entry.Time.IsZero() {
			return prediction.DemandResult{}, errors.New("Linear CPU history requires timestamps")
		}
	}

	sort.Slice(trainingHistory, func(i, j int) bool {
		return trainingHistory[i].Time.Before(trainingHistory[j].Time)
	})

	parameters, err := json.Marshal(linearRegressionParameters{
		ReturnTrajectory: model.PredictivePolicy != nil,
		LookAhead:        model.Linear.LookAhead,
		ReplicaHistory:   trainingHistory,
		// Match the sample-based expiry policy and avoid extrapolating across
		// queue or process startup delays. metav1.Time uses the history wire format.
		CurrentTime: trainingHistory[len(trainingHistory)-1].Time,
	})
	if err != nil {
		return prediction.DemandResult{}, err
	}

	timeout := defaultTimeout
	if model.CalculationTimeout != nil {
		timeout = *model.CalculationTimeout
	}

	ctx, trajectory := algorithm.WithForecastTrajectory(ctx)
	value, err := algorithm.RunCPUForecast(ctx, p.Runner, algorithmPath, string(parameters), timeout)
	if err != nil {
		return prediction.DemandResult{}, err
	}

	predictedUsage, err := parsePredictedCPUUsage(value)
	if err != nil {
		return prediction.DemandResult{}, err
	}

	return prediction.CompleteCPUForecast(model, prediction.DemandResult{
		Trajectory:    trajectory.Last(),
		Demand:        prediction.Demand{Kind: request.Observation.Kind, Target: request.Observation.Target, Value: predictedUsage},
		ConsumedUntil: prediction.LatestTimestamp(trainingHistory),
	})
}

func (p *Predict) PruneHistory(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) ([]hpaplusv1alpha1.TimestampedSample, error) {
	if model.Linear == nil {
		return nil, errors.New("no Linear configuration provided for model")
	}

	if model.Linear.HistorySize < 0 {
		return nil, errors.New("historySize must be nonnegative for pruning")
	}
	cpuHistoryCount := prediction.CountCPUHistory(replicaHistory)
	if cpuHistoryCount == 0 {
		replicaHistory = prediction.CloneHistory(replicaHistory)
		if len(replicaHistory) <= model.Linear.HistorySize {
			return replicaHistory, nil
		}

		sort.Slice(replicaHistory, func(i, j int) bool {
			return !replicaHistory[i].Time.Before(replicaHistory[j].Time)
		})

		for i := len(replicaHistory) - 1; i >= model.Linear.HistorySize; i-- {
			replicaHistory = append(replicaHistory[:i], replicaHistory[i+1:]...)
		}

		return replicaHistory, nil
	}

	return prediction.PruneCPUHistory(replicaHistory, model.Linear.HistorySize), nil
}

// GetType returns the type of the Prediction model
func (p *Predict) GetType() string {
	return hpaplusv1alpha1.TypeLinear
}

func parsePredictedCPUUsage(value string) (int64, error) {
	return prediction.ParseCPUForecast(value, "Linear regression")
}

// PrepareAdaptation confirms that the next normal prediction refits from the
// configured rolling history. It does not shorten the model's history window.
func (p *Predict) PrepareAdaptation(ctx context.Context, model *hpaplusv1alpha1.Model) (string, error) {
	return "Refit", ctx.Err()
}
