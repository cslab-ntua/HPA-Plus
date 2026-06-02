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

package arima

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	ctrlLog "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	defaultTimeout = 30000
)

const algorithmPath = "algorithms/arima/arima.py"
const incrementalWorkerPath = "algorithms/arima/incremental_worker.py"

type arimaParameters struct {
	Order                []int                                         `json:"order"`
	LookAhead            int                                           `json:"lookAhead"`
	ReplicaHistory       []hpaplusv1alpha1.TimestampedReplicas `json:"replicaHistory"`
	Trend                *string                                       `json:"trend,omitempty"`
	AutoArima            bool                                          `json:"autoArima"`
	InformationCriterion string                                        `json:"informationCriterion"`
	MaxOrder             []int                                         `json:"maxOrder,omitempty"`
	EnforceStationarity  bool                                          `json:"enforceStationarity"`
	EnforceInvertibility bool                                          `json:"enforceInvertibility"`
	ConcentrateScale     bool                                          `json:"concentrateScale"`
	UseSarima            bool                                          `json:"useSarima"`
	SeasonalOrder        []int                                         `json:"seasonalOrder,omitempty"`
	SeasonalPeriods      *int                                          `json:"seasonalPeriods,omitempty"`
}

// Config represents an ARIMA prediction model configuration
type Config struct {
	Order                []int   `yaml:"order"`
	LookAhead            int     `yaml:"lookAhead"`
	Trend                *string `yaml:"trend,omitempty"`
	AutoArima            bool    `yaml:"autoArima"`
	InformationCriterion string  `yaml:"informationCriterion"`
	MaxOrder             []int   `yaml:"maxOrder,omitempty"`
	EnforceStationarity  bool    `yaml:"enforceStationarity"`
	EnforceInvertibility bool    `yaml:"enforceInvertibility"`
	ConcentrateScale     bool    `yaml:"concentrateScale"`
	UseSarima            bool    `yaml:"useSarima"`
	SeasonalOrder        []int   `yaml:"seasonalOrder,omitempty"`
	SeasonalPeriods      *int    `yaml:"seasonalPeriods,omitempty"`
}

// Runner defines an algorithm runner, allowing algorithms to be run
type AlgorithmRunner interface {
	RunAlgorithmWithValue(algorithmPath string, value string, timeout int) (string, error)
}

// IncrementalRunner defines a stateful algorithm runner that keeps long-lived worker sessions.
type IncrementalRunner interface {
	RunSessionWithValue(sessionID, algorithmPath, value string, timeout int) (string, error)
	ResetSession(sessionID string) error
}

type incrementalRequest struct {
	Action         string                                        `json:"action"`
	Config         *arimaParameters                              `json:"config,omitempty"`
	ReplicaHistory []hpaplusv1alpha1.TimestampedReplicas `json:"replicaHistory,omitempty"`
}

type incrementalResponse struct {
	Ok         bool   `json:"ok"`
	Prediction *int   `json:"prediction,omitempty"`
	Error      string `json:"error,omitempty"`
}

type incrementalState struct {
	ConfigHash        string
	LastProcessedTime *time.Time
	UpdatesSinceRefit int
	Initialized       bool
}

// Predict provides logic for using ARIMA to make a prediction
type Predict struct {
	Runner            AlgorithmRunner
	IncrementalRunner IncrementalRunner

	stateMu              sync.Mutex
	incrementalStateByID map[string]*incrementalState
}

// GetPrediction uses ARIMA to predict what the replica count should be based on historical evaluations
func (p *Predict) GetPrediction(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedReplicas) (int32, error) {
	result, err := p.GetPredictionResult(model, replicaHistory)
	if err != nil {
		return 0, err
	}
	return result.Replicas, nil
}

// GetPredictionResult uses ARIMA to predict what the replica count should be based on historical evaluations.
func (p *Predict) GetPredictionResult(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedReplicas) (prediction.Result, error) {
	if model.Arima == nil {
		return prediction.Result{}, errors.New("no ARIMA configuration provided for model")
	}

	trainingHistory := filterHistoryForTraining(model, replicaHistory)
	if len(trainingHistory) == 0 {
		return prediction.Result{}, errors.New("no CPU usage evaluations provided for ARIMA model")
	}

	// ARIMA requires at least 3 data points to work properly
	if len(trainingHistory) < 3 {
		// Return the most recent replica count as a fallback
		return prediction.Result{
			Replicas:      trainingHistory[len(trainingHistory)-1].Replicas,
			ConsumedUntil: prediction.LatestTimestamp(trainingHistory),
		}, nil
	}

	var replicas int32
	var err error
	if !p.incrementalEnabled(model) || p.IncrementalRunner == nil {
		replicas, err = p.getPredictionOneShot(model, trainingHistory)
	} else {
		replicas, err = p.getPredictionIncremental(model, trainingHistory)
	}
	if err != nil {
		return prediction.Result{}, err
	}

	return prediction.Result{
		Replicas:      replicas,
		ConsumedUntil: prediction.LatestTimestamp(trainingHistory),
	}, nil
}

func (p *Predict) PruneHistory(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedReplicas) ([]hpaplusv1alpha1.TimestampedReplicas, error) {
	if model.Arima == nil {
		return nil, errors.New("no ARIMA configuration provided for model")
	}

	// Default history size for ARIMA if not specified
	defaultHistorySize := 50
	if model.Arima.HistorySize != nil {
		defaultHistorySize = *model.Arima.HistorySize
	}

	trainingHistoryCount := countTrainingEntries(model, replicaHistory)
	if trainingHistoryCount == 0 {
		if len(replicaHistory) <= defaultHistorySize {
			return replicaHistory, nil
		}

		start := len(replicaHistory) - defaultHistorySize
		if start < 0 {
			start = 0
		}
		return replicaHistory[start:], nil
	}

	if trainingHistoryCount <= defaultHistorySize {
		return replicaHistory, nil
	}

	return pruneHistoryByTrainingEntries(model, replicaHistory, defaultHistorySize), nil
}

// GetType returns the type of the Prediction model
func (p *Predict) GetType() string {
	return hpaplusv1alpha1.TypeArima
}

func (p *Predict) getPredictionOneShot(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedReplicas) (int32, error) {
	parametersStruct := p.buildParameters(model, replicaHistory)
	parameters, err := json.Marshal(parametersStruct)
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
		return 0, err
	}

	predictedUsage, err := parsePredictedCPUUsage(value)
	if err != nil {
		return 0, err
	}

	replicas, err := convertPredictedCPUUsageToReplicas(model, predictedUsage)
	if err != nil {
		return 0, err
	}
	logRawForecast(model, predictedUsage, replicas)
	return replicas, nil
}

func (p *Predict) getPredictionIncremental(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedReplicas) (int32, error) {
	if hasMissingTimestamps(replicaHistory) {
		// Incremental updates require ordering by timestamps. Fallback to one-shot if timestamps are missing.
		return p.getPredictionOneShot(model, replicaHistory)
	}

	timeout := defaultTimeout
	if model.CalculationTimeout != nil {
		timeout = *model.CalculationTimeout
	}

	sessionID := model.Name
	if model.SessionID != "" {
		sessionID = model.SessionID
	}
	parameters := p.buildParameters(model, replicaHistory)
	configHash := hashParameters(parameters)
	latestHistoryTime := getLatestHistoryTime(replicaHistory)
	if latestHistoryTime == nil {
		return p.getPredictionOneShot(model, replicaHistory)
	}

	state := p.getOrCreateState(sessionID)
	refitEvery := 0
	if model.Arima.RefitEvery != nil {
		refitEvery = *model.Arima.RefitEvery
	}

	needsFullRefit := !state.Initialized || state.ConfigHash != configHash
	if !needsFullRefit && refitEvery > 0 && state.UpdatesSinceRefit >= refitEvery {
		needsFullRefit = true
	}

	if needsFullRefit {
		predictedUsage, err := p.runIncremental(sessionID, incrementalRequest{
			Action:         "fit_forecast",
			Config:         &parameters,
			ReplicaHistory: replicaHistory,
		}, timeout)
		if err != nil {
			p.resetState(sessionID)
			return p.getPredictionOneShot(model, replicaHistory)
		}

		p.updateState(sessionID, func(s *incrementalState) {
			s.Initialized = true
			s.ConfigHash = configHash
			s.LastProcessedTime = latestHistoryTime
			s.UpdatesSinceRefit = 0
		})
		replicas, err := convertPredictedCPUUsageToReplicas(model, predictedUsage)
		if err != nil {
			return 0, err
		}
		logRawForecast(model, predictedUsage, replicas)
		return replicas, nil
	}

	newEntries := filterHistoryAfter(replicaHistory, state.LastProcessedTime)
	request := incrementalRequest{
		Action: "forecast",
		Config: &parameters,
	}
	if len(newEntries) > 0 {
		request.Action = "append_forecast"
		request.ReplicaHistory = newEntries
	}

	predictedUsage, err := p.runIncremental(sessionID, request, timeout)
	if err != nil {
		p.resetState(sessionID)
		return p.getPredictionOneShot(model, replicaHistory)
	}

	p.updateState(sessionID, func(s *incrementalState) {
		s.ConfigHash = configHash
		s.Initialized = true
		if len(newEntries) > 0 {
			s.LastProcessedTime = getLatestHistoryTime(newEntries)
			s.UpdatesSinceRefit += len(newEntries)
		}
	})

	replicas, err := convertPredictedCPUUsageToReplicas(model, predictedUsage)
	if err != nil {
		return 0, err
	}
	logRawForecast(model, predictedUsage, replicas)
	return replicas, nil
}

func (p *Predict) runIncremental(sessionID string, request incrementalRequest, timeout int) (int64, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return 0, err
	}

	responseRaw, err := p.IncrementalRunner.RunSessionWithValue(sessionID, incrementalWorkerPath, string(body), timeout)
	if err != nil {
		return 0, err
	}

	response := incrementalResponse{}
	err = json.Unmarshal([]byte(responseRaw), &response)
	if err != nil {
		return 0, fmt.Errorf("failed to decode incremental ARIMA response: %w", err)
	}
	if !response.Ok {
		if response.Error == "" {
			return 0, errors.New("incremental ARIMA worker returned failure without an error message")
		}
		return 0, errors.New(response.Error)
	}
	if response.Prediction == nil {
		return 0, errors.New("incremental ARIMA worker returned no prediction")
	}

	return int64(*response.Prediction), nil
}

func (p *Predict) incrementalEnabled(model *hpaplusv1alpha1.Model) bool {
	if model.Arima == nil || model.Arima.IncrementalUpdates == nil {
		return false
	}
	return *model.Arima.IncrementalUpdates
}

func (p *Predict) buildParameters(model *hpaplusv1alpha1.Model,
	replicaHistory []hpaplusv1alpha1.TimestampedReplicas) arimaParameters {
	autoArima := false
	informationCriterion := "aic"
	enforceStationarity := true
	enforceInvertibility := true
	concentrateScale := false
	useSarima := false

	if model.Arima.AutoArima != nil {
		autoArima = *model.Arima.AutoArima
	}
	if model.Arima.InformationCriterion != nil {
		informationCriterion = *model.Arima.InformationCriterion
	}
	if model.Arima.EnforceStationarity != nil {
		enforceStationarity = *model.Arima.EnforceStationarity
	}
	if model.Arima.EnforceInvertibility != nil {
		enforceInvertibility = *model.Arima.EnforceInvertibility
	}
	if model.Arima.ConcentrateScale != nil {
		concentrateScale = *model.Arima.ConcentrateScale
	}
	if model.Arima.UseSarima != nil {
		useSarima = *model.Arima.UseSarima
	}

	return arimaParameters{
		Order:                model.Arima.Order,
		LookAhead:            model.Arima.LookAhead,
		ReplicaHistory:       replicaHistory,
		Trend:                model.Arima.Trend,
		AutoArima:            autoArima,
		InformationCriterion: informationCriterion,
		MaxOrder:             model.Arima.MaxOrder,
		EnforceStationarity:  enforceStationarity,
		EnforceInvertibility: enforceInvertibility,
		ConcentrateScale:     concentrateScale,
		UseSarima:            useSarima,
		SeasonalOrder:        model.Arima.SeasonalOrder,
		SeasonalPeriods:      model.Arima.SeasonalPeriods,
	}
}

func hashParameters(parameters arimaParameters) string {
	copy := parameters
	copy.ReplicaHistory = nil
	body, err := json.Marshal(copy)
	if err != nil {
		panic(err)
	}
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:])
}

func hasMissingTimestamps(replicaHistory []hpaplusv1alpha1.TimestampedReplicas) bool {
	for _, entry := range replicaHistory {
		if entry.Time == nil {
			return true
		}
	}
	return false
}

func getLatestHistoryTime(replicaHistory []hpaplusv1alpha1.TimestampedReplicas) *time.Time {
	var latest *time.Time
	for _, entry := range replicaHistory {
		if entry.Time == nil {
			continue
		}
		t := entry.Time.Time
		if latest == nil || t.After(*latest) {
			clone := t
			latest = &clone
		}
	}
	return latest
}

func filterHistoryAfter(replicaHistory []hpaplusv1alpha1.TimestampedReplicas,
	lastProcessed *time.Time) []hpaplusv1alpha1.TimestampedReplicas {
	if lastProcessed == nil {
		return replicaHistory
	}

	filtered := make([]hpaplusv1alpha1.TimestampedReplicas, 0, len(replicaHistory))
	for _, entry := range replicaHistory {
		if entry.Time == nil {
			continue
		}
		if entry.Time.Time.After(*lastProcessed) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func (p *Predict) getOrCreateState(sessionID string) *incrementalState {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.incrementalStateByID == nil {
		p.incrementalStateByID = map[string]*incrementalState{}
	}
	state, exists := p.incrementalStateByID[sessionID]
	if !exists {
		state = &incrementalState{}
		p.incrementalStateByID[sessionID] = state
	}
	return state
}

func (p *Predict) updateState(sessionID string, updateFn func(state *incrementalState)) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.incrementalStateByID == nil {
		p.incrementalStateByID = map[string]*incrementalState{}
	}
	state, exists := p.incrementalStateByID[sessionID]
	if !exists {
		state = &incrementalState{}
		p.incrementalStateByID[sessionID] = state
	}
	updateFn(state)
}

func (p *Predict) resetState(sessionID string) {
	_ = p.IncrementalRunner.ResetSession(sessionID)
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.incrementalStateByID != nil {
		delete(p.incrementalStateByID, sessionID)
	}
}

func filterHistoryForTraining(
	model *hpaplusv1alpha1.Model,
	replicaHistory []hpaplusv1alpha1.TimestampedReplicas,
) []hpaplusv1alpha1.TimestampedReplicas {
	filtered := make([]hpaplusv1alpha1.TimestampedReplicas, 0, len(replicaHistory))
	for _, entry := range replicaHistory {
		if !trainingEntryUsable(model, entry) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func countTrainingEntries(
	model *hpaplusv1alpha1.Model,
	replicaHistory []hpaplusv1alpha1.TimestampedReplicas,
) int {
	count := 0
	for _, entry := range replicaHistory {
		if trainingEntryUsable(model, entry) {
			count++
		}
	}
	return count
}

func pruneHistoryByTrainingEntries(
	model *hpaplusv1alpha1.Model,
	replicaHistory []hpaplusv1alpha1.TimestampedReplicas,
	maxTrainingEntries int,
) []hpaplusv1alpha1.TimestampedReplicas {
	if maxTrainingEntries <= 0 {
		return []hpaplusv1alpha1.TimestampedReplicas{}
	}

	trainingEntriesSeen := 0
	start := len(replicaHistory)
	for idx := len(replicaHistory) - 1; idx >= 0; idx-- {
		if trainingEntryUsable(model, replicaHistory[idx]) {
			trainingEntriesSeen++
		}
		if trainingEntriesSeen >= maxTrainingEntries {
			start = idx
			break
		}
	}

	if start <= 0 {
		return replicaHistory
	}

	return replicaHistory[start:]
}

func trainingEntryUsable(model *hpaplusv1alpha1.Model,
	entry hpaplusv1alpha1.TimestampedReplicas) bool {
	if entry.TotalCPUUsageMillicores == nil {
		return false
	}
	return true
}

func parsePredictedCPUUsage(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("empty ARIMA prediction output")
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
		return 0, errors.New("missing CPU request per pod for ARIMA CPU-history prediction")
	}
	if model.TargetCPUUtilizationPercentage <= 0 {
		return 0, errors.New("missing target CPU utilization for ARIMA CPU-history prediction")
	}

	if predictedUsage < 0 {
		predictedUsage = 0
	}

	targetPerPod := float64(model.CPURequestPerPodMillicores) * (float64(model.TargetCPUUtilizationPercentage) / 100.0)
	if targetPerPod <= 0 {
		return 0, errors.New("invalid CPU target conversion values for ARIMA CPU-history prediction")
	}

	return int32(math.Ceil(float64(predictedUsage) / targetPerPod)), nil
}

func logRawForecast(model *hpaplusv1alpha1.Model, predictedUsage int64, predictedReplicas int32) {
	sessionID := model.Name
	if model.SessionID != "" {
		sessionID = model.SessionID
	}

	ctrlLog.Log.WithName("arima").Info(
		"ARIMA raw CPU forecast ready",
		"sessionID", sessionID,
		"modelName", model.Name,
		"predictedUsageMillicores", predictedUsage,
		"predictedReplicas", predictedReplicas,
	)
}
