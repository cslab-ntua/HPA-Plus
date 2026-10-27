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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
)

const (
	defaultTimeout = hpaplusv1alpha1.DefaultCalculationTimeoutMillis
)

const algorithmPath = "algorithms/arima/arima.py"
const incrementalWorkerPath = "algorithms/arima/incremental_worker.py"

type arimaParameters struct {
	ReturnTrajectory     bool                                `json:"returnTrajectory,omitempty"`
	Order                []int                               `json:"order"`
	LookAhead            int                                 `json:"lookAhead"`
	ReplicaHistory       []hpaplusv1alpha1.TimestampedSample `json:"replicaHistory,omitempty"`
	Trend                *string                             `json:"trend,omitempty"`
	AutoArima            bool                                `json:"autoArima"`
	InformationCriterion string                              `json:"informationCriterion"`
	MaxOrder             []int                               `json:"maxOrder,omitempty"`
	EnforceStationarity  bool                                `json:"enforceStationarity"`
	EnforceInvertibility bool                                `json:"enforceInvertibility"`
	ConcentrateScale     bool                                `json:"concentrateScale"`
	UseSarima            bool                                `json:"useSarima"`
	SeasonalOrder        []int                               `json:"seasonalOrder,omitempty"`
	SeasonalPeriods      *int                                `json:"seasonalPeriods,omitempty"`
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
	ProtocolVersion    int                                 `json:"protocolVersion"`
	DiagnosticsVersion int                                 `json:"diagnosticsVersion,omitempty"`
	Unit               string                              `json:"unit"`
	RequestID          string                              `json:"requestId"`
	Action             string                              `json:"action"`
	Config             *arimaParameters                    `json:"config,omitempty"`
	ReplicaHistory     []hpaplusv1alpha1.TimestampedSample `json:"replicaHistory,omitempty"`
}

type incrementalResponse struct {
	ProtocolVersion int    `json:"protocolVersion"`
	Unit            string `json:"unit"`
	RequestID       string `json:"requestId"`
	Ok              *bool  `json:"ok"`
	Prediction      *int64 `json:"prediction,omitempty"`
	Error           string `json:"error,omitempty"`
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
func (p *Predict) GetPrediction(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) (int32, error) {
	result, err := p.GetPredictionResult(model, replicaHistory)
	if err != nil {
		return 0, err
	}
	return result.Replicas, nil
}

// GetPredictionResult uses ARIMA to predict what the replica count should be based on historical evaluations.
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
	if model.Arima == nil {
		return prediction.DemandResult{}, errors.New("no ARIMA configuration provided for model")
	}

	trainingHistory := prediction.FilterCPUHistory(replicaHistory)
	if len(trainingHistory) == 0 {
		return prediction.DemandResult{}, prediction.ErrInsufficientHistory
	}

	// At least three observations are needed before fitting ARIMA.
	if len(trainingHistory) < 3 {
		return prediction.DemandResult{}, prediction.ErrInsufficientHistory
	}

	ctx, trajectory := algorithm.WithForecastTrajectory(ctx)
	var usage int64
	var err error
	if !p.incrementalEnabled(model) || p.IncrementalRunner == nil {
		usage, err = p.getPredictionOneShot(ctx, model, trainingHistory)
	} else {
		usage, err = p.getPredictionIncremental(ctx, model, trainingHistory)
	}
	if err != nil {
		return prediction.DemandResult{}, err
	}

	return prediction.CompleteCPUForecast(model, prediction.DemandResult{
		Trajectory:    trajectory.Last(),
		Demand:        prediction.Demand{Kind: request.Observation.Kind, Target: request.Observation.Target, Value: usage},
		ConsumedUntil: prediction.LatestTimestamp(trainingHistory),
	})
}

func (p *Predict) PruneHistory(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) ([]hpaplusv1alpha1.TimestampedSample, error) {
	if model.Arima == nil {
		return nil, errors.New("no ARIMA configuration provided for model")
	}

	// Default history size for ARIMA if not specified
	defaultHistorySize := hpaplusv1alpha1.DefaultARIMAHistorySize
	if model.Arima.HistorySize != nil {
		defaultHistorySize = *model.Arima.HistorySize
	}

	if defaultHistorySize < 0 {
		return nil, errors.New("historySize must be nonnegative for pruning")
	}
	trainingHistoryCount := prediction.CountCPUHistory(replicaHistory)
	if trainingHistoryCount == 0 {
		replicaHistory = prediction.CloneHistory(replicaHistory)
		if len(replicaHistory) <= defaultHistorySize {
			return replicaHistory, nil
		}

		start := len(replicaHistory) - defaultHistorySize
		if start < 0 {
			start = 0
		}
		return replicaHistory[start:], nil
	}

	return prediction.PruneCPUHistory(replicaHistory, defaultHistorySize), nil
}

// GetType returns the type of the Prediction model
func (p *Predict) GetType() string {
	return hpaplusv1alpha1.TypeArima
}

func (p *Predict) getPredictionOneShot(ctx context.Context, model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) (int64, error) {
	parametersStruct := p.buildParameters(model, replicaHistory)
	parameters, err := json.Marshal(parametersStruct)
	if err != nil {
		return 0, fmt.Errorf("encode ARIMA parameters: %w", err)
	}

	timeout := defaultTimeout
	if model.CalculationTimeout != nil {
		timeout = *model.CalculationTimeout
	}

	value, err := algorithm.RunCPUForecast(ctx, p.Runner, algorithmPath, string(parameters), timeout)
	if err != nil {
		return 0, err
	}

	predictedUsage, err := parsePredictedCPUUsage(value)
	if err != nil {
		return 0, err
	}

	return predictedUsage, nil
}

func (p *Predict) getPredictionIncremental(ctx context.Context, model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) (int64, error) {
	if hasMissingTimestamps(replicaHistory) {
		// Incremental updates require ordering by timestamps. Fallback to one-shot if timestamps are missing.
		return p.getPredictionOneShot(ctx, model, replicaHistory)
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
	configHash, err := hashParameters(parameters)
	if err != nil {
		return 0, err
	}
	latestHistoryTime := getLatestHistoryTime(replicaHistory)
	if latestHistoryTime == nil {
		return p.getPredictionOneShot(ctx, model, replicaHistory)
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
		predictedUsage, err := p.runIncremental(ctx, sessionID, incrementalRequest{
			Action:         "fit_forecast",
			Config:         &parameters,
			ReplicaHistory: replicaHistory,
		}, timeout)
		if err != nil {
			p.resetState(sessionID)
			return p.getPredictionOneShot(ctx, model, replicaHistory)
		}

		p.updateState(sessionID, func(s *incrementalState) {
			s.Initialized = true
			s.ConfigHash = configHash
			s.LastProcessedTime = latestHistoryTime
			s.UpdatesSinceRefit = 0
		})
		return predictedUsage, nil
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

	predictedUsage, err := p.runIncremental(ctx, sessionID, request, timeout)
	if err != nil {
		p.resetState(sessionID)
		return p.getPredictionOneShot(ctx, model, replicaHistory)
	}

	p.updateState(sessionID, func(s *incrementalState) {
		s.ConfigHash = configHash
		s.Initialized = true
		if len(newEntries) > 0 {
			s.LastProcessedTime = getLatestHistoryTime(newEntries)
			s.UpdatesSinceRefit += len(newEntries)
		}
	})

	return predictedUsage, nil
}

func (p *Predict) runIncremental(ctx context.Context, sessionID string, request incrementalRequest, timeout int) (int64, error) {
	if request.Config != nil {
		// History belongs to the action payload. Repeating the full training
		// window inside config defeats incremental append/forecast efficiency.
		config := *request.Config
		config.ReplicaHistory = nil
		request.Config = &config
	}
	request = versionedRequest(request)
	if algorithm.WantsForecastDiagnostics(ctx) {
		request.DiagnosticsVersion = 1
	}
	body, err := json.Marshal(request)
	if err != nil {
		return 0, err
	}

	responseRaw, err := algorithm.RunSession(ctx, p.IncrementalRunner, sessionID, incrementalWorkerPath, string(body), timeout)
	if err != nil {
		return 0, err
	}

	return algorithm.DecodeCPUForecastResponseContext(ctx, responseRaw, request.RequestID)
}

func (p *Predict) incrementalEnabled(model *hpaplusv1alpha1.Model) bool {
	if model.Arima == nil || model.Arima.IncrementalUpdates == nil {
		return false
	}
	return *model.Arima.IncrementalUpdates
}

func (p *Predict) buildParameters(model *hpaplusv1alpha1.Model,
	replicaHistory []hpaplusv1alpha1.TimestampedSample) arimaParameters {
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
		ReturnTrajectory:     model.PredictivePolicy != nil,
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

func hashParameters(parameters arimaParameters) (string, error) {
	copy := parameters
	copy.ReplicaHistory = nil
	body, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("encode ARIMA configuration hash: %w", err)
	}
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:]), nil
}

func hasMissingTimestamps(replicaHistory []hpaplusv1alpha1.TimestampedSample) bool {
	for _, entry := range replicaHistory {
		if entry.Time == nil {
			return true
		}
	}
	return false
}

func getLatestHistoryTime(replicaHistory []hpaplusv1alpha1.TimestampedSample) *time.Time {
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

func filterHistoryAfter(replicaHistory []hpaplusv1alpha1.TimestampedSample,
	lastProcessed *time.Time) []hpaplusv1alpha1.TimestampedSample {
	if lastProcessed == nil {
		return replicaHistory
	}

	filtered := make([]hpaplusv1alpha1.TimestampedSample, 0, len(replicaHistory))
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
	copy := *state
	return &copy
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

func (p *Predict) SyncSessions(prefix string, active map[string]bool) {
	p.stateMu.Lock()
	var removed []string
	for id := range p.incrementalStateByID {
		if strings.HasPrefix(id, prefix) && !active[id] {
			delete(p.incrementalStateByID, id)
			removed = append(removed, id)
		}
	}
	p.stateMu.Unlock()
	if p.IncrementalRunner != nil {
		for _, id := range removed {
			_ = p.IncrementalRunner.ResetSession(id)
		}
	}
}

func parsePredictedCPUUsage(value string) (int64, error) {
	return prediction.ParseCPUForecast(value, "ARIMA")
}

// PrepareAdaptation discards incremental fitted state, not training observations.
// The next forecast fits the configured retained history from scratch.
func (p *Predict) PrepareAdaptation(ctx context.Context, model *hpaplusv1alpha1.Model) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.IncrementalRunner != nil && model.SessionID != "" {
		if err := p.IncrementalRunner.ResetSession(model.SessionID); err != nil {
			return "", err
		}
		p.stateMu.Lock()
		delete(p.incrementalStateByID, model.SessionID)
		p.stateMu.Unlock()
	}
	return "Refit", nil
}
