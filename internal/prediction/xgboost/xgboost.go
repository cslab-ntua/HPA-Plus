package xgboost

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"

	jamiethompsonmev1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	ctrlLog "sigs.k8s.io/controller-runtime/pkg/log"
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

// GetPrediction uses XGBoost to predict aggregate CPU usage based on historical evaluations
// and converts that prediction back into a replica count.
func (p *Predict) GetPrediction(model *jamiethompsonmev1alpha1.Model, replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) (int32, error) {
	result, err := p.GetPredictionResult(model, replicaHistory)
	if err != nil {
		return 0, err
	}
	return result.Replicas, nil
}

// GetPredictionResult uses XGBoost to predict aggregate CPU usage based on historical evaluations
// and converts that prediction back into a replica count.
func (p *Predict) GetPredictionResult(model *jamiethompsonmev1alpha1.Model, replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) (prediction.Result, error) {
	if model.XGBoost == nil {
		return prediction.Result{}, errors.New("no XGBoost configuration provided for model")
	}

	trainingHistory := filterHistoryWithCPUUsage(replicaHistory)
	if len(trainingHistory) == 0 {
		return prediction.Result{}, errors.New("no CPU usage evaluations provided for XGBoost model")
	}

	if len(trainingHistory) == 1 {
		return prediction.Result{
			Replicas:      trainingHistory[0].Replicas,
			ConsumedUntil: prediction.LatestTimestamp(trainingHistory),
		}, nil
	}

	metrics := extractMetricHistory(trainingHistory)

	params, err := json.Marshal(xgboostParameters{
		LookAhead:       model.XGBoost.LookAhead,
		Lags:            model.XGBoost.Lags,
		ReplicaHistory:  trainingHistory,
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

// PruneHistory ensures replica history does not exceed configured history size
func (p *Predict) PruneHistory(model *jamiethompsonmev1alpha1.Model, replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) ([]jamiethompsonmev1alpha1.TimestampedReplicas, error) {
	if model.XGBoost == nil {
		return nil, errors.New("no XGBoost configuration provided for model")
	}

	cpuHistoryCount := countCPUUsageEntries(replicaHistory)
	if cpuHistoryCount == 0 {
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

	if cpuHistoryCount <= model.XGBoost.HistorySize {
		return replicaHistory, nil
	}

	return pruneHistoryByCPUUsage(replicaHistory, model.XGBoost.HistorySize), nil
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

func filterHistoryWithCPUUsage(replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) []jamiethompsonmev1alpha1.TimestampedReplicas {
	filtered := make([]jamiethompsonmev1alpha1.TimestampedReplicas, 0, len(replicaHistory))
	for _, entry := range replicaHistory {
		if entry.TotalCPUUsageMillicores == nil {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func countCPUUsageEntries(replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) int {
	count := 0
	for _, entry := range replicaHistory {
		if entry.TotalCPUUsageMillicores != nil {
			count++
		}
	}
	return count
}

func pruneHistoryByCPUUsage(
	replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas,
	maxCPUEntries int,
) []jamiethompsonmev1alpha1.TimestampedReplicas {
	if maxCPUEntries <= 0 {
		return []jamiethompsonmev1alpha1.TimestampedReplicas{}
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
		return 0, errors.New("empty XGBoost prediction output")
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

func convertPredictedCPUUsageToReplicas(model *jamiethompsonmev1alpha1.Model, predictedUsage int64) (int32, error) {
	if model.CPURequestPerPodMillicores <= 0 {
		return 0, errors.New("missing CPU request per pod for XGBoost CPU-history prediction")
	}
	if model.TargetCPUUtilizationPercentage <= 0 {
		return 0, errors.New("missing target CPU utilization for XGBoost CPU-history prediction")
	}

	if predictedUsage < 0 {
		predictedUsage = 0
	}

	targetPerPod := float64(model.CPURequestPerPodMillicores) * (float64(model.TargetCPUUtilizationPercentage) / 100.0)
	if targetPerPod <= 0 {
		return 0, errors.New("invalid CPU target conversion values for XGBoost CPU-history prediction")
	}

	return int32(math.Ceil(float64(predictedUsage) / targetPerPod)), nil
}

func logRawForecast(model *jamiethompsonmev1alpha1.Model, predictedUsage int64, predictedReplicas int32) {
	sessionID := model.Name
	if model.SessionID != "" {
		sessionID = model.SessionID
	}

	ctrlLog.Log.WithName("xgboost").Info(
		"XGBoost raw CPU forecast ready",
		"sessionID", sessionID,
		"modelName", model.Name,
		"predictedUsageMillicores", predictedUsage,
		"predictedReplicas", predictedReplicas,
	)
}
