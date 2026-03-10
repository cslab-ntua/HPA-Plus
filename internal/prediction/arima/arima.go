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
	"strconv"
	"sync"
	"time"

	jamiethompsonmev1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

const (
	defaultTimeout = 30000
)

const algorithmPath = "algorithms/arima/arima.py"
const incrementalWorkerPath = "algorithms/arima/incremental_worker.py"

type arimaParameters struct {
	Order                []int                                         `json:"order"`
	LookAhead            int                                           `json:"lookAhead"`
	ReplicaHistory       []jamiethompsonmev1alpha1.TimestampedReplicas `json:"replicaHistory"`
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
	ReplicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas `json:"replicaHistory,omitempty"`
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
func (p *Predict) GetPrediction(model *jamiethompsonmev1alpha1.Model, replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) (int32, error) {
	if model.Arima == nil {
		return 0, errors.New("no ARIMA configuration provided for model")
	}

	if len(replicaHistory) == 0 {
		return 0, errors.New("no evaluations provided for ARIMA model")
	}

	// ARIMA requires at least 3 data points to work properly
	if len(replicaHistory) < 3 {
		// Return the most recent replica count as a fallback
		return replicaHistory[len(replicaHistory)-1].Replicas, nil
	}

	if !p.incrementalEnabled(model) || p.IncrementalRunner == nil {
		return p.getPredictionOneShot(model, replicaHistory)
	}

	return p.getPredictionIncremental(model, replicaHistory)
}

func (p *Predict) PruneHistory(model *jamiethompsonmev1alpha1.Model, replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) ([]jamiethompsonmev1alpha1.TimestampedReplicas, error) {
	if model.Arima == nil {
		return nil, errors.New("no ARIMA configuration provided for model")
	}

	// Default history size for ARIMA if not specified
	defaultHistorySize := 50
	if model.Arima.HistorySize != nil {
		defaultHistorySize = *model.Arima.HistorySize
	}

	if len(replicaHistory) <= defaultHistorySize {
		return replicaHistory, nil
	}

	// Keep only the newest HistorySize observations while preserving chronological order
	start := len(replicaHistory) - defaultHistorySize
	if start < 0 {
		start = 0
	}
	replicaHistory = replicaHistory[start:]

	return replicaHistory, nil
}

// GetType returns the type of the Prediction model
func (p *Predict) GetType() string {
	return jamiethompsonmev1alpha1.TypeArima
}

func (p *Predict) getPredictionOneShot(model *jamiethompsonmev1alpha1.Model, replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) (int32, error) {
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

	prediction, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
	}

	return int32(prediction), nil
}

func (p *Predict) getPredictionIncremental(model *jamiethompsonmev1alpha1.Model, replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) (int32, error) {
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
		prediction, err := p.runIncremental(sessionID, incrementalRequest{
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
		return prediction, nil
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

	prediction, err := p.runIncremental(sessionID, request, timeout)
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

	return prediction, nil
}

func (p *Predict) runIncremental(sessionID string, request incrementalRequest, timeout int) (int32, error) {
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

	return int32(*response.Prediction), nil
}

func (p *Predict) incrementalEnabled(model *jamiethompsonmev1alpha1.Model) bool {
	if model.Arima == nil || model.Arima.IncrementalUpdates == nil {
		return false
	}
	return *model.Arima.IncrementalUpdates
}

func (p *Predict) buildParameters(model *jamiethompsonmev1alpha1.Model,
	replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) arimaParameters {
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

func hasMissingTimestamps(replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) bool {
	for _, entry := range replicaHistory {
		if entry.Time == nil {
			return true
		}
	}
	return false
}

func getLatestHistoryTime(replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) *time.Time {
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

func filterHistoryAfter(replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas,
	lastProcessed *time.Time) []jamiethompsonmev1alpha1.TimestampedReplicas {
	if lastProcessed == nil {
		return replicaHistory
	}

	filtered := make([]jamiethompsonmev1alpha1.TimestampedReplicas, 0, len(replicaHistory))
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
