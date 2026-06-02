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

package holtwinters

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/hook"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	ctrlLog "sigs.k8s.io/controller-runtime/pkg/log"
)

const algorithmPath = "algorithms/holt_winters/holt_winters.py"

const (
	defaultTimeout = 30000
)

// Runner defines an algorithm runner, allowing algorithms to be run
type AlgorithmRunner interface {
	RunAlgorithmWithValue(algorithmPath string, value string, timeout int) (string, error)
}

// Predict provides logic for using Holt Winters to make a prediction
type Predict struct {
	HookExecute hook.Executer
	Runner      AlgorithmRunner
}

type holtWintersParametersParameters struct {
	Series               []float64 `json:"series"`
	Alpha                float64   `json:"alpha"`
	Beta                 float64   `json:"beta"`
	Gamma                float64   `json:"gamma"`
	Trend                string    `json:"trend"`
	Seasonal             string    `json:"seasonal"`
	SeasonalPeriods      int       `json:"seasonalPeriods"`
	DampedTrend          *bool     `json:"dampedTrend,omitempty"`
	InitializationMethod *string   `json:"initializationMethod,omitempty"`
	InitialLevel         *float64  `json:"initialLevel,omitempty"`
	InitialTrend         *float64  `json:"initialTrend,omitempty"`
	InitialSeasonal      *float64  `json:"initialSeasonal,omitempty"`
}

type runTimeTuningFetchHookRequest struct {
	Model          hpaplusv1alpha1.Model                 `json:"model"`
	ReplicaHistory []hpaplusv1alpha1.TimestampedReplicas `json:"replicaHistory"`
}

type runTimeTuningFetchHookResult struct {
	Alpha *float64 `json:"alpha"`
	Beta  *float64 `json:"beta"`
	Gamma *float64 `json:"gamma"`
}

// GetPrediction uses Holt-Winters to predict aggregate CPU usage and converts that prediction
// back into a replica count.
func (p *Predict) GetPrediction(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedReplicas) (int32, error) {
	result, err := p.GetPredictionResult(model, replicaHistory)
	if err != nil {
		return 0, err
	}
	return result.Replicas, nil
}

// GetPredictionResult uses Holt-Winters to predict aggregate CPU usage and converts that
// prediction back into a replica count.
func (p *Predict) GetPredictionResult(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedReplicas) (prediction.Result, error) {
	err := p.validate(model)
	if err != nil {
		return prediction.Result{}, err
	}

	trainingHistory := filterHistoryWithCPUUsage(replicaHistory)
	if len(trainingHistory) == 0 {
		return prediction.Result{}, errors.New("no CPU usage evaluations provided for Holt-Winters model")
	}

	sort.Slice(trainingHistory, func(i, j int) bool {
		return trainingHistory[i].Time.Before(trainingHistory[j].Time)
	})

	// Statsmodels requires at least 2 * seasonal_periods to make a prediction with Holt Winters
	// https://github.com/statsmodels/statsmodels/blob/77bb1d276c7d11bc8657497b4307aa7575c3e65c/statsmodels/tsa/exponential_smoothing/initialization.py#L57-L61
	if len(trainingHistory) < 2*model.HoltWinters.SeasonalPeriods {
		return prediction.Result{}, nil
	}

	// Statsmodels requires at least 10 + 2 * (seasonal_periods // 2) to make a prediction with Holt Winters
	// https://github.com/statsmodels/statsmodels/blob/77bb1d276c7d11bc8657497b4307aa7575c3e65c/statsmodels/tsa/exponential_smoothing/initialization.py#L66-L71
	if len(trainingHistory) < 10+2*(model.HoltWinters.SeasonalPeriods/2) {
		return prediction.Result{}, nil
	}

	alpha := model.HoltWinters.Alpha
	beta := model.HoltWinters.Beta
	gamma := model.HoltWinters.Gamma

	if model.HoltWinters.RuntimeTuningFetchHook != nil {

		// Convert request into JSON string
		request, err := json.Marshal(&runTimeTuningFetchHookRequest{
			Model:          *model,
			ReplicaHistory: trainingHistory,
		})
		if err != nil {
			// Should not occur
			panic(err)
		}

		// Request runtime tuning values
		hookResult, err := p.HookExecute.ExecuteWithValue(model.HoltWinters.RuntimeTuningFetchHook, string(request))
		if err != nil {
			return prediction.Result{}, err
		}

		// Parse result
		var result runTimeTuningFetchHookResult
		err = json.Unmarshal([]byte(hookResult), &result)
		if err != nil {
			return prediction.Result{}, err
		}

		if result.Alpha != nil {
			alpha = result.Alpha
		}
		if result.Beta != nil {
			beta = result.Beta
		}
		if result.Gamma != nil {
			gamma = result.Gamma
		}
	}

	if alpha == nil {
		return prediction.Result{}, errors.New("no alpha tuning value provided for Holt-Winters prediction")
	}
	if beta == nil {
		return prediction.Result{}, errors.New("no beta tuning value provided for Holt-Winters prediction")
	}
	if gamma == nil {
		return prediction.Result{}, errors.New("no gamma tuning value provided for Holt-Winters prediction")
	}

	// Collect data for historical series
	series := make([]float64, len(trainingHistory))
	for i, timestampedReplica := range trainingHistory {
		series[i] = float64(*timestampedReplica.TotalCPUUsageMillicores)
	}

	parameters, err := json.Marshal(holtWintersParametersParameters{
		Series:               series,
		Alpha:                *alpha,
		Beta:                 *beta,
		Gamma:                *gamma,
		Trend:                model.HoltWinters.Trend,
		Seasonal:             model.HoltWinters.Seasonal,
		SeasonalPeriods:      model.HoltWinters.SeasonalPeriods,
		DampedTrend:          model.HoltWinters.DampedTrend,
		InitializationMethod: model.HoltWinters.InitializationMethod,
		InitialLevel:         model.HoltWinters.InitialLevel,
		InitialTrend:         model.HoltWinters.InitialTrend,
		InitialSeasonal:      model.HoltWinters.InitialSeasonal,
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
	err := p.validate(model)
	if err != nil {
		return nil, err
	}

	cpuHistoryCount := countCPUUsageEntries(replicaHistory)
	if cpuHistoryCount == 0 {
		// Sort by date created, oldest first to preserve chronological order
		sort.Slice(replicaHistory, func(i, j int) bool {
			return replicaHistory[i].Time.Before(replicaHistory[j].Time)
		})

		seasonLength := model.HoltWinters.SeasonalPeriods
		numberOfSeasons := len(replicaHistory) / seasonLength
		numberOfSeasonsToRemove := numberOfSeasons - model.HoltWinters.StoredSeasons
		if numberOfSeasonsToRemove <= 0 {
			return replicaHistory, nil
		}

		numberOfReplicasToRemove := numberOfSeasonsToRemove * seasonLength
		if numberOfReplicasToRemove >= len(replicaHistory) {
			return []hpaplusv1alpha1.TimestampedReplicas{}, nil
		}

		return replicaHistory[numberOfReplicasToRemove:], nil
	}

	// Sort by date created, oldest first to preserve chronological order
	sort.Slice(replicaHistory, func(i, j int) bool {
		return replicaHistory[i].Time.Before(replicaHistory[j].Time)
	})

	seasonLength := model.HoltWinters.SeasonalPeriods
	numberOfSeasons := cpuHistoryCount / seasonLength
	numberOfSeasonsToRemove := numberOfSeasons - model.HoltWinters.StoredSeasons
	if numberOfSeasonsToRemove <= 0 {
		return replicaHistory, nil
	}

	maxCPUEntries := model.HoltWinters.StoredSeasons * seasonLength
	return pruneHistoryByCPUUsage(replicaHistory, maxCPUEntries), nil
}

// GetType returns the type of the Prediction model
func (p *Predict) GetType() string {
	return hpaplusv1alpha1.TypeHoltWinters
}

func (p *Predict) validate(model *hpaplusv1alpha1.Model) error {
	if model.HoltWinters == nil {
		return errors.New("no HoltWinters configuration provided for model")
	}

	if model.HoltWinters.Trend == "" {
		return errors.New("no required 'trend' value provided for model")
	}

	return nil
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
		return 0, errors.New("empty Holt-Winters prediction output")
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
		return 0, errors.New("missing CPU request per pod for Holt-Winters CPU-history prediction")
	}
	if model.TargetCPUUtilizationPercentage <= 0 {
		return 0, errors.New("missing target CPU utilization for Holt-Winters CPU-history prediction")
	}

	if predictedUsage < 0 {
		predictedUsage = 0
	}

	targetPerPod := float64(model.CPURequestPerPodMillicores) * (float64(model.TargetCPUUtilizationPercentage) / 100.0)
	if targetPerPod <= 0 {
		return 0, errors.New("invalid CPU target conversion values for Holt-Winters CPU-history prediction")
	}

	return int32(math.Ceil(float64(predictedUsage) / targetPerPod)), nil
}

func logRawForecast(model *hpaplusv1alpha1.Model, predictedUsage int64, predictedReplicas int32) {
	sessionID := model.Name
	if model.SessionID != "" {
		sessionID = model.SessionID
	}

	ctrlLog.Log.WithName("holtwinters").Info(
		"Holt-Winters raw CPU forecast ready",
		"sessionID", sessionID,
		"modelName", model.Name,
		"predictedUsageMillicores", predictedUsage,
		"predictedReplicas", predictedReplicas,
	)
}
