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
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
	"github.com/cslab-ntua/HPA-Plus/internal/hook"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
)

const algorithmPath = "algorithms/holt_winters/holt_winters.py"

const (
	defaultTimeout = hpaplusv1alpha1.DefaultCalculationTimeoutMillis
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
	ReturnTrajectory     bool     `json:"returnTrajectory,omitempty"`
	LookAhead            *int     `json:"lookAhead,omitempty"`
	Timestamps           []string `json:"timestamps,omitempty"`
	Series               []int64  `json:"series"`
	Alpha                float64  `json:"alpha"`
	Beta                 float64  `json:"beta"`
	Gamma                float64  `json:"gamma"`
	Trend                string   `json:"trend"`
	Seasonal             string   `json:"seasonal"`
	SeasonalPeriods      int      `json:"seasonalPeriods"`
	DampedTrend          *bool    `json:"dampedTrend,omitempty"`
	InitializationMethod *string  `json:"initializationMethod,omitempty"`
	InitialLevel         *float64 `json:"initialLevel,omitempty"`
	InitialTrend         *float64 `json:"initialTrend,omitempty"`
	InitialSeasonal      *float64 `json:"initialSeasonal,omitempty"`
}

type runTimeTuningFetchHookRequest struct {
	Model          hpaplusv1alpha1.Model               `json:"model"`
	ReplicaHistory []hpaplusv1alpha1.TimestampedSample `json:"replicaHistory"`
}

type runTimeTuningFetchHookResult struct {
	Alpha *float64 `json:"alpha"`
	Beta  *float64 `json:"beta"`
	Gamma *float64 `json:"gamma"`
}

// GetPrediction uses Holt-Winters to predict aggregate CPU usage and converts that prediction
// back into a replica count.
func (p *Predict) GetPrediction(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) (int32, error) {
	result, err := p.GetPredictionResult(model, replicaHistory)
	if err != nil {
		return 0, err
	}
	return result.Replicas, nil
}

// GetPredictionResult uses Holt-Winters to predict aggregate CPU usage and converts that
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
	err := p.validate(model)
	if err != nil {
		return prediction.DemandResult{}, err
	}

	trainingHistory := prediction.FilterCPUHistory(replicaHistory)
	if len(trainingHistory) == 0 {
		return prediction.DemandResult{}, prediction.ErrInsufficientHistory
	}

	sort.Slice(trainingHistory, func(i, j int) bool {
		return trainingHistory[i].Time.Before(trainingHistory[j].Time)
	})

	// Statsmodels requires at least 2 * seasonal_periods to make a prediction with Holt Winters
	// https://github.com/statsmodels/statsmodels/blob/77bb1d276c7d11bc8657497b4307aa7575c3e65c/statsmodels/tsa/exponential_smoothing/initialization.py#L57-L61
	if len(trainingHistory) < 2*model.HoltWinters.SeasonalPeriods {
		return prediction.DemandResult{}, prediction.ErrInsufficientHistory
	}

	// Statsmodels requires at least 10 + 2 * (seasonal_periods // 2) to make a prediction with Holt Winters
	// https://github.com/statsmodels/statsmodels/blob/77bb1d276c7d11bc8657497b4307aa7575c3e65c/statsmodels/tsa/exponential_smoothing/initialization.py#L66-L71
	if len(trainingHistory) < 10+2*(model.HoltWinters.SeasonalPeriods/2) {
		return prediction.DemandResult{}, prediction.ErrInsufficientHistory
	}

	alpha := model.HoltWinters.Alpha
	beta := model.HoltWinters.Beta
	gamma := model.HoltWinters.Gamma

	if model.HoltWinters.RuntimeTuningFetchHook != nil {

		// Never send hook URLs, headers or credential configuration as model data.
		requestModel := model.DeepCopy()
		requestModel.HoltWinters.RuntimeTuningFetchHook = nil

		// Convert request into JSON string
		request, err := json.Marshal(&runTimeTuningFetchHookRequest{
			Model:          *requestModel,
			ReplicaHistory: trainingHistory,
		})
		if err != nil {
			return prediction.DemandResult{}, err
		}

		// Request runtime tuning values
		hookResult, err := hook.Execute(ctx, p.HookExecute, model.HoltWinters.RuntimeTuningFetchHook, string(request))
		if err != nil {
			return prediction.DemandResult{}, err
		}

		// Parse result
		var result runTimeTuningFetchHookResult
		err = json.Unmarshal([]byte(hookResult), &result)
		if err != nil {
			return prediction.DemandResult{}, err
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
		return prediction.DemandResult{}, errors.New("no alpha tuning value provided for Holt-Winters prediction")
	}
	if beta == nil {
		return prediction.DemandResult{}, errors.New("no beta tuning value provided for Holt-Winters prediction")
	}
	if gamma == nil {
		return prediction.DemandResult{}, errors.New("no gamma tuning value provided for Holt-Winters prediction")
	}
	for _, value := range []float64{*alpha, *beta, *gamma} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			return prediction.DemandResult{}, errors.New("Holt-Winters tuning values must be finite and in [0, 1]")
		}
	}

	// Collect data for historical series
	series := make([]int64, len(trainingHistory))
	timestamps := make([]string, len(trainingHistory))
	for i, timestampedReplica := range trainingHistory {
		series[i] = *timestampedReplica.TotalCPUUsageMillicores
		if timestampedReplica.Time == nil || timestampedReplica.Time.IsZero() {
			if model.HoltWinters.LookAhead != nil {
				return prediction.DemandResult{}, errors.New("Holt-Winters lookAhead requires timestamped history")
			}
			timestamps = nil
		} else if timestamps != nil {
			timestamps[i] = timestampedReplica.Time.UTC().Format(time.RFC3339Nano)
		}
	}

	parameters, err := json.Marshal(holtWintersParametersParameters{
		ReturnTrajectory:     model.PredictivePolicy != nil,
		Series:               series,
		LookAhead:            model.HoltWinters.LookAhead,
		Timestamps:           timestamps,
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
	err := p.validate(model)
	if err != nil {
		return nil, err
	}

	if model.HoltWinters.SeasonalPeriods <= 0 || model.HoltWinters.StoredSeasons < 0 ||
		model.HoltWinters.StoredSeasons > math.MaxInt/model.HoltWinters.SeasonalPeriods {
		return nil, errors.New("invalid or overflowing seasonal history window")
	}
	replicaHistory = prediction.CloneHistory(replicaHistory)
	cpuHistoryCount := prediction.CountCPUHistory(replicaHistory)
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
			return []hpaplusv1alpha1.TimestampedSample{}, nil
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
	return prediction.PruneCPUHistory(replicaHistory, maxCPUEntries), nil
}

// GetType returns the type of the Prediction model
func (p *Predict) GetType() string {
	return hpaplusv1alpha1.TypeHoltWinters
}

func (p *Predict) validate(model *hpaplusv1alpha1.Model) error {
	if model.HoltWinters == nil {
		return errors.New("no HoltWinters configuration provided for model")
	}

	if model.HoltWinters.LookAhead != nil && (*model.HoltWinters.LookAhead < 1 || int64(*model.HoltWinters.LookAhead) > math.MaxInt64/int64(time.Millisecond)) {
		return errors.New("invalid Holt-Winters lookAhead duration")
	}
	if model.HoltWinters.Trend == "" {
		return errors.New("no required 'trend' value provided for model")
	}

	return nil
}

func parsePredictedCPUUsage(value string) (int64, error) {
	return prediction.ParseCPUForecast(value, "Holt-Winters")
}

// PrepareAdaptation confirms that the next normal prediction refits from the
// configured rolling history. It does not shorten the model's history window.
func (p *Predict) PrepareAdaptation(ctx context.Context, model *hpaplusv1alpha1.Model) (string, error) {
	return "Refit", ctx.Err()
}
