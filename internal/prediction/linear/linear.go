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
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	ctrlLog "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	defaultTimeout = 30000
)

const algorithmPath = "algorithms/linear_regression/linear_regression.py"

type linearRegressionParameters struct {
	LookAhead      int                                           `json:"lookAhead"`
	ReplicaHistory []hpaplusv1alpha1.TimestampedReplicas `json:"replicaHistory"`
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
func (p *Predict) GetPrediction(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedReplicas) (int32, error) {
	result, err := p.GetPredictionResult(model, replicaHistory)
	if err != nil {
		return 0, err
	}
	return result.Replicas, nil
}

// GetPredictionResult uses linear regression to predict aggregate CPU usage and converts that
// prediction back into a replica count.
func (p *Predict) GetPredictionResult(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedReplicas) (prediction.Result, error) {
	if model.Linear == nil {
		return prediction.Result{}, errors.New("no Linear configuration provided for model")
	}

	trainingHistory := filterHistoryWithCPUUsage(replicaHistory)
	if len(trainingHistory) == 0 {
		return prediction.Result{}, errors.New("no CPU usage evaluations provided for Linear regression model")
	}

	sort.Slice(trainingHistory, func(i, j int) bool {
		return trainingHistory[i].Time.Before(trainingHistory[j].Time)
	})

	if len(trainingHistory) == 1 {
		// If only 1 evaluation is provided do not try and calculate using the linear regression model, just return
		// the target replicas from the only evaluation.
		return prediction.Result{
			Replicas:      trainingHistory[0].Replicas,
			ConsumedUntil: prediction.LatestTimestamp(trainingHistory),
		}, nil
	}

	parameters, err := json.Marshal(linearRegressionParameters{
		LookAhead:      model.Linear.LookAhead,
		ReplicaHistory: trainingHistory,
	})
	if err != nil {
		// Should not occur, panic
		panic(err)
	}

	timeout := defaultTimeout
	if model.CalculationTimeout != nil {
		timeout = *model.CalculationTimeout
	}

	value, err := p.Runner.RunAlgorithmWithValue(algorithmPath, string(parameters), timeout)
	if err != nil {
		return prediction.Result{}, err
	}

	predictedUsage, err := parsePredictedCPUUsage(value)
	if err != nil {
		return prediction.Result{}, err
	}

	predictedReplicas, err := convertPredictedCPUUsageToReplicas(model, predictedUsage)
	if err != nil {
		return prediction.Result{}, err
	}
	logRawForecast(model, predictedUsage, predictedReplicas)

	return prediction.Result{
		Replicas:      predictedReplicas,
		ConsumedUntil: prediction.LatestTimestamp(trainingHistory),
	}, nil
}

func (p *Predict) PruneHistory(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedReplicas) ([]hpaplusv1alpha1.TimestampedReplicas, error) {
	if model.Linear == nil {
		return nil, errors.New("no Linear configuration provided for model")
	}

	cpuHistoryCount := countCPUUsageEntries(replicaHistory)
	if cpuHistoryCount == 0 {
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

	if cpuHistoryCount <= model.Linear.HistorySize {
		return replicaHistory, nil
	}

	// Keep the newest CPU-backed samples even if the input history ordering is disturbed.
	sort.Slice(replicaHistory, func(i, j int) bool {
		return replicaHistory[i].Time.Before(replicaHistory[j].Time)
	})

	return pruneHistoryByCPUUsage(replicaHistory, model.Linear.HistorySize), nil
}

// GetType returns the type of the Prediction model
func (p *Predict) GetType() string {
	return hpaplusv1alpha1.TypeLinear
}

func filterHistoryWithCPUUsage(replicaHistory []hpaplusv1alpha1.TimestampedReplicas) []hpaplusv1alpha1.TimestampedReplicas {
	filtered := make([]hpaplusv1alpha1.TimestampedReplicas, 0, len(replicaHistory))
	for _, entry := range replicaHistory {
		if entry.TotalCPUUsageMillicores == nil {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func countCPUUsageEntries(replicaHistory []hpaplusv1alpha1.TimestampedReplicas) int {
	count := 0
	for _, entry := range replicaHistory {
		if entry.TotalCPUUsageMillicores != nil {
			count++
		}
	}
	return count
}

func pruneHistoryByCPUUsage(
	replicaHistory []hpaplusv1alpha1.TimestampedReplicas,
	maxCPUEntries int,
) []hpaplusv1alpha1.TimestampedReplicas {
	if maxCPUEntries <= 0 {
		return []hpaplusv1alpha1.TimestampedReplicas{}
	}

	cpuEntriesSeen := 0
	start := len(replicaHistory)
	for idx := len(replicaHistory) - 1; idx >= 0; idx-- {
		if replicaHistory[idx].TotalCPUUsageMillicores != nil {
			cpuEntriesSeen++
		}
		if cpuEntriesSeen >= maxCPUEntries {
			start = idx
			break
		}
	}

	if start <= 0 {
		return replicaHistory
	}

	return replicaHistory[start:]
}

func parsePredictedCPUUsage(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("empty Linear regression prediction output")
	}

	predictedUsage, err := strconv.ParseInt(value, 10, 64)
	if err == nil {
		return predictedUsage, nil
	}

	predictedUsageFloat, floatErr := strconv.ParseFloat(value, 64)
	if floatErr != nil {
		return 0, err
	}

	return int64(math.Ceil(predictedUsageFloat)), nil
}

func convertPredictedCPUUsageToReplicas(model *hpaplusv1alpha1.Model, predictedUsage int64) (int32, error) {
	if model.CPURequestPerPodMillicores <= 0 {
		return 0, errors.New("missing CPU request per pod for Linear CPU-history prediction")
	}
	if model.TargetCPUUtilizationPercentage <= 0 {
		return 0, errors.New("missing target CPU utilization for Linear CPU-history prediction")
	}

	if predictedUsage < 0 {
		predictedUsage = 0
	}

	targetPerPod := float64(model.CPURequestPerPodMillicores) * (float64(model.TargetCPUUtilizationPercentage) / 100.0)
	if targetPerPod <= 0 {
		return 0, errors.New("invalid CPU target conversion values for Linear CPU-history prediction")
	}

	return int32(math.Ceil(float64(predictedUsage) / targetPerPod)), nil
}

func logRawForecast(model *hpaplusv1alpha1.Model, predictedUsage int64, predictedReplicas int32) {
	sessionID := model.Name
	if model.SessionID != "" {
		sessionID = model.SessionID
	}

	ctrlLog.Log.WithName("linear").Info(
		"Linear raw CPU forecast ready",
		"sessionID", sessionID,
		"modelName", model.Name,
		"predictedUsageMillicores", predictedUsage,
		"predictedReplicas", predictedReplicas,
	)
}
