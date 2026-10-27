package xgboost

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
)

const (
	defaultTimeout = hpaplusv1alpha1.DefaultCalculationTimeoutMillis
)

const algorithmPath = "algorithms/xgboost/xgboost.py"

type xgboostParameters struct {
	ReturnTrajectory                 bool                                `json:"returnTrajectory,omitempty"`
	ForecastStrategy                 string                              `json:"forecastStrategy,omitempty"`
	SeasonalFeatures                 bool                                `json:"seasonalFeatures,omitempty"`
	ForecastAggregation              string                              `json:"forecastAggregation,omitempty"`
	DiagnosticScope                  string                              `json:"diagnosticScope,omitempty"`
	LookAhead                        int                                 `json:"lookAhead"`
	Lags                             int                                 `json:"lags"`
	ReplicaHistory                   []hpaplusv1alpha1.TimestampedSample `json:"replicaHistory"`
	MetricHistory                    []float64                           `json:"metricHistory,omitempty"`
	WindowSize                       *int                                `json:"windowSize,omitempty"`
	MaxDepth                         *int                                `json:"maxDepth,omitempty"`
	NEstimators                      *int                                `json:"nEstimators,omitempty"`
	LearningRate                     *float64                            `json:"learningRate,omitempty"`
	Subsample                        *float64                            `json:"subsample,omitempty"`
	ColsampleBytree                  *float64                            `json:"colsampleBytree,omitempty"`
	Objective                        *string                             `json:"objective,omitempty"`
	QuantileAlpha                    *float64                            `json:"quantileAlpha,omitempty"`
	CPUDemandWeightExponent          *float64                            `json:"cpuDemandWeightExponent,omitempty"`
	AFTLossDistributionScale         *float64                            `json:"aftLossDistributionScale,omitempty"`
	CPUCensoringUtilizationThreshold *float64                            `json:"cpuCensoringUtilizationThreshold,omitempty"`
	MinChildWeight                   *float64                            `json:"minChildWeight,omitempty"`
	Gamma                            *float64                            `json:"gamma,omitempty"`
	RegLambda                        *float64                            `json:"regLambda,omitempty"`
	RegAlpha                         *float64                            `json:"regAlpha,omitempty"`
}

// AlgorithmRunner defines an algorithm runner, allowing algorithms to be run
type AlgorithmRunner interface {
	RunAlgorithmWithValue(algorithmPath string, value string, timeout int) (string, error)
}

// Predict provides logic for using XGBoost to make a prediction
type Predict struct {
	Runner AlgorithmRunner
}

// GetPrediction uses XGBoost to predict aggregate CPU usage based on historical evaluations
// and converts that prediction back into a replica count.
func (p *Predict) GetPrediction(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) (int32, error) {
	result, err := p.GetPredictionResult(model, replicaHistory)
	if err != nil {
		return 0, err
	}
	return result.Replicas, nil
}

// GetPredictionResult uses XGBoost to predict aggregate CPU usage based on historical evaluations
// and converts that prediction back into a replica count.
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
	if model.XGBoost == nil {
		return prediction.DemandResult{}, errors.New("no XGBoost configuration provided for model")
	}

	trainingHistory := prediction.FilterCPUHistory(replicaHistory)
	if len(trainingHistory) == 0 {
		return prediction.DemandResult{}, prediction.ErrInsufficientHistory
	}

	if len(trainingHistory) < 2 || len(trainingHistory) <= model.XGBoost.Lags {
		return prediction.DemandResult{}, prediction.ErrInsufficientHistory
	}

	metrics := prediction.MetricHistory(trainingHistory)
	if model.XGBoost.UseUtilizationFeatures != nil && !*model.XGBoost.UseUtilizationFeatures {
		if model.XGBoost.Objective != nil && *model.XGBoost.Objective == "survival:aft" {
			return prediction.DemandResult{}, errors.New("censored CPU forecasting requires utilization features")
		}
		metrics = nil
	}
	if model.XGBoost.Objective != nil && *model.XGBoost.Objective == "survival:aft" && len(metrics) != len(trainingHistory) {
		return prediction.DemandResult{}, errors.New("censored CPU forecasting requires complete utilization history")
	}

	params, err := json.Marshal(xgboostParameters{
		ReturnTrajectory:                 model.PredictivePolicy != nil || (model.ForecastReliability != nil && model.XGBoost.ForecastStrategy == "direct"),
		ForecastStrategy:                 model.XGBoost.ForecastStrategy,
		SeasonalFeatures:                 model.XGBoost.SeasonalFeatures,
		ForecastAggregation:              model.XGBoost.ForecastAggregation,
		DiagnosticScope:                  prediction.DiagnosticScope(model),
		LookAhead:                        model.XGBoost.LookAhead,
		Lags:                             model.XGBoost.Lags,
		ReplicaHistory:                   trainingHistory,
		MetricHistory:                    metrics,
		WindowSize:                       model.XGBoost.WindowSize,
		MaxDepth:                         model.XGBoost.MaxDepth,
		NEstimators:                      model.XGBoost.NEstimators,
		LearningRate:                     model.XGBoost.LearningRate,
		Subsample:                        model.XGBoost.Subsample,
		ColsampleBytree:                  model.XGBoost.ColsampleBytree,
		Objective:                        model.XGBoost.Objective,
		QuantileAlpha:                    model.XGBoost.QuantileAlpha,
		CPUDemandWeightExponent:          model.XGBoost.CPUDemandWeightExponent,
		AFTLossDistributionScale:         model.XGBoost.AFTLossDistributionScale,
		CPUCensoringUtilizationThreshold: model.XGBoost.CPUCensoringUtilizationThreshold,
		MinChildWeight:                   model.XGBoost.MinChildWeight,
		Gamma:                            model.XGBoost.Gamma,
		RegLambda:                        model.XGBoost.RegLambda,
		RegAlpha:                         model.XGBoost.RegAlpha,
	})
	if err != nil {
		return prediction.DemandResult{}, err
	}

	timeout := defaultTimeout
	if model.CalculationTimeout != nil {
		timeout = *model.CalculationTimeout
	}

	ctx, trajectory := algorithm.WithForecastTrajectory(ctx)
	value, err := algorithm.RunCPUForecast(ctx, p.Runner, algorithmPath, string(params), timeout)
	if err != nil {
		return prediction.DemandResult{}, err
	}

	predictedUsage, err := parsePredictedCPUUsage(value)
	if err != nil {
		return prediction.DemandResult{}, err
	}

	points := trajectory.Last()
	return prediction.CompleteCPUForecast(model, prediction.DemandResult{
		Trajectory:    points,
		Demand:        prediction.Demand{Kind: request.Observation.Kind, Target: request.Observation.Target, Value: predictedUsage},
		ConsumedUntil: prediction.LatestTimestamp(trainingHistory),
	})
}

// PruneHistory ensures replica history does not exceed configured history size
func (p *Predict) PruneHistory(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) ([]hpaplusv1alpha1.TimestampedSample, error) {
	if model.XGBoost == nil {
		return nil, errors.New("no XGBoost configuration provided for model")
	}

	if model.XGBoost.HistorySize < 0 {
		return nil, errors.New("historySize must be nonnegative for pruning")
	}
	cpuHistoryCount := prediction.CountCPUHistory(replicaHistory)
	if cpuHistoryCount == 0 {
		replicaHistory = prediction.CloneHistory(replicaHistory)
		if len(replicaHistory) <= model.XGBoost.HistorySize {
			return replicaHistory, nil
		}

		sort.Slice(replicaHistory, func(i, j int) bool {
			return !replicaHistory[i].Time.Before(replicaHistory[j].Time)
		})

		for i := len(replicaHistory) - 1; i >= model.XGBoost.HistorySize; i-- {
			replicaHistory = append(replicaHistory[:i], replicaHistory[i+1:]...)
		}

		return replicaHistory, nil
	}

	return prediction.PruneCPUHistory(replicaHistory, model.XGBoost.HistorySize), nil
}

// GetType returns the type of the Prediction model
func (p *Predict) GetType() string {
	return hpaplusv1alpha1.TypeXGBoost
}

func parsePredictedCPUUsage(value string) (int64, error) {
	return prediction.ParseCPUForecast(value, "XGBoost")
}

// PrepareAdaptation confirms that the next normal prediction refits from the
// configured rolling history. It does not shorten the model's history window.
func (p *Predict) PrepareAdaptation(ctx context.Context, model *hpaplusv1alpha1.Model) (string, error) {
	return "Refit", ctx.Err()
}
