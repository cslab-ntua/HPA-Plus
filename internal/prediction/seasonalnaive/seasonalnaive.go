package seasonalnaive

import (
	"context"
	"encoding/json"
	"errors"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
)

const (
	defaultTimeout = hpaplusv1alpha1.DefaultCalculationTimeoutMillis
)

const algorithmPath = "algorithms/seasonal_naive/seasonal_naive.py"

type parameters struct {
	ReturnTrajectory      bool                                `json:"returnTrajectory,omitempty"`
	LookAhead             int                                 `json:"lookAhead"`
	SeasonalPeriodSeconds *int                                `json:"seasonalPeriodSeconds,omitempty"`
	ReplicaHistory        []hpaplusv1alpha1.TimestampedSample `json:"replicaHistory"`
}

// AlgorithmRunner defines an algorithm runner, allowing algorithms to be run.
type AlgorithmRunner interface {
	RunAlgorithmWithValue(algorithmPath string, value string, timeout int) (string, error)
}

// Predict repeats the CPU usage of one cycle earlier.
type Predict struct {
	Runner AlgorithmRunner
}

func (p *Predict) GetPrediction(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) (int32, error) {
	result, err := p.GetPredictionResult(model, replicaHistory)
	if err != nil {
		return 0, err
	}
	return result.Replicas, nil
}

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
	if model.SeasonalNaive == nil {
		return prediction.DemandResult{}, errors.New("no seasonal naive configuration provided for model")
	}

	history := prediction.FilterCPUHistory(replicaHistory)
	if len(history) < 3 {
		return prediction.DemandResult{}, prediction.ErrInsufficientHistory
	}

	params, err := json.Marshal(parameters{
		ReturnTrajectory:      model.PredictivePolicy != nil,
		LookAhead:             model.SeasonalNaive.LookAhead,
		SeasonalPeriodSeconds: model.SeasonalNaive.SeasonalPeriodSeconds,
		ReplicaHistory:        history,
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
	usage, err := prediction.ParseCPUForecast(value, "SeasonalNaive")
	if err != nil {
		return prediction.DemandResult{}, err
	}
	return prediction.CompleteCPUForecast(model, prediction.DemandResult{
		Trajectory:    trajectory.Last(),
		Demand:        prediction.Demand{Kind: request.Observation.Kind, Target: request.Observation.Target, Value: usage},
		ConsumedUntil: prediction.LatestTimestamp(history),
	})
}

// PruneHistory keeps the configured number of CPU samples.
func (p *Predict) PruneHistory(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) ([]hpaplusv1alpha1.TimestampedSample, error) {
	if model.SeasonalNaive == nil {
		return nil, errors.New("no seasonal naive configuration provided for model")
	}
	if model.SeasonalNaive.HistorySize < 0 {
		return nil, errors.New("historySize must be nonnegative for pruning")
	}
	return prediction.PruneCPUHistory(replicaHistory, model.SeasonalNaive.HistorySize), nil
}

func (p *Predict) GetType() string {
	return hpaplusv1alpha1.TypeSeasonalNaive
}

// PrepareAdaptation needs no refit: every forecast reads the newest history.
func (p *Predict) PrepareAdaptation(ctx context.Context, model *hpaplusv1alpha1.Model) (string, error) {
	return "Refit", ctx.Err()
}
