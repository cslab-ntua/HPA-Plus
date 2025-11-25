package xgboost

import (
	"encoding/json"
	"errors"
	"sort"
	"strconv"

	jamiethompsonmev1alpha1 "github.com/jthomperoo/predictive-horizontal-pod-autoscaler/api/v1alpha1"
)

const (
	defaultTimeout = 30000
)

const algorithmPath = "algorithms/xgboost/xgboost.py"

type xgboostParameters struct {
	LookAhead       int                                           `json:"lookAhead"`
	Lags            int                                           `json:"lags"`
	ReplicaHistory  []jamiethompsonmev1alpha1.TimestampedReplicas `json:"replicaHistory"`
	MetricHistory   []float64                                     `json:"metricHistory,omitempty"`
	WindowSize      *int                                          `json:"windowSize,omitempty"`
	MaxDepth        *int                                          `json:"maxDepth,omitempty"`
	NEstimators     *int                                          `json:"nEstimators,omitempty"`
	LearningRate    *float64                                      `json:"learningRate,omitempty"`
	Subsample       *float64                                      `json:"subsample,omitempty"`
	ColsampleBytree *float64                                      `json:"colsampleBytree,omitempty"`
	Objective       *string                                       `json:"objective,omitempty"`
	MinChildWeight  *float64                                      `json:"minChildWeight,omitempty"`
	Gamma           *float64                                      `json:"gamma,omitempty"`
	RegLambda       *float64                                      `json:"regLambda,omitempty"`
	RegAlpha        *float64                                      `json:"regAlpha,omitempty"`
}

// AlgorithmRunner defines an algorithm runner, allowing algorithms to be run
type AlgorithmRunner interface {
	RunAlgorithmWithValue(algorithmPath string, value string, timeout int) (string, error)
}

// Predict provides logic for using XGBoost to make a prediction
type Predict struct {
	Runner AlgorithmRunner
}

// GetPrediction uses XGBoost to predict what the replica count should be based on historical evaluations
func (p *Predict) GetPrediction(model *jamiethompsonmev1alpha1.Model, replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) (int32, error) {
	if model.XGBoost == nil {
		return 0, errors.New("no XGBoost configuration provided for model")
	}

	if len(replicaHistory) == 0 {
		return 0, errors.New("no evaluations provided for XGBoost model")
	}

	if len(replicaHistory) == 1 {
		return replicaHistory[0].Replicas, nil
	}

	metrics := extractMetricHistory(replicaHistory)

	params, err := json.Marshal(xgboostParameters{
		LookAhead:       model.XGBoost.LookAhead,
		Lags:            model.XGBoost.Lags,
		ReplicaHistory:  replicaHistory,
		MetricHistory:   metrics,
		WindowSize:      model.XGBoost.WindowSize,
		MaxDepth:        model.XGBoost.MaxDepth,
		NEstimators:     model.XGBoost.NEstimators,
		LearningRate:    model.XGBoost.LearningRate,
		Subsample:       model.XGBoost.Subsample,
		ColsampleBytree: model.XGBoost.ColsampleBytree,
		Objective:       model.XGBoost.Objective,
		MinChildWeight:  model.XGBoost.MinChildWeight,
		Gamma:           model.XGBoost.Gamma,
		RegLambda:       model.XGBoost.RegLambda,
		RegAlpha:        model.XGBoost.RegAlpha,
	})
	if err != nil {
		panic(err)
	}

	timeout := defaultTimeout
	if model.CalculationTimeout != nil {
		timeout = *model.CalculationTimeout
	}

	value, err := p.Runner.RunAlgorithmWithValue(algorithmPath, string(params), timeout)
	if err != nil {
		return 0, err
	}

	prediction, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
	}

	return int32(prediction), nil
}

// PruneHistory ensures replica history does not exceed configured history size
func (p *Predict) PruneHistory(model *jamiethompsonmev1alpha1.Model, replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) ([]jamiethompsonmev1alpha1.TimestampedReplicas, error) {
	if model.XGBoost == nil {
		return nil, errors.New("no XGBoost configuration provided for model")
	}

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

// GetType returns the type of the Prediction model
func (p *Predict) GetType() string {
	return jamiethompsonmev1alpha1.TypeXGBoost
}

func extractMetricHistory(replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) []float64 {
	metrics := make([]float64, 0, len(replicaHistory))
	for _, entry := range replicaHistory {
		if entry.Metric == nil {
			return nil
		}
		metrics = append(metrics, *entry.Metric)
	}

	if len(metrics) == 0 {
		return nil
	}

	return metrics
}
