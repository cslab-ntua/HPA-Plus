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
	"encoding/json"
	"errors"
	"strconv"

	jamiethompsonmev1alpha1 "github.com/jthomperoo/predictive-horizontal-pod-autoscaler/api/v1alpha1"
)

const (
	defaultTimeout = 30000
)

const algorithmPath = "algorithms/arima/arima.py"

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

// Predict provides logic for using ARIMA to make a prediction
type Predict struct {
	Runner AlgorithmRunner
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

	// Set default values for ARIMA configuration
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

	parameters, err := json.Marshal(arimaParameters{
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
		return 0, err
	}

	prediction, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
	}

	return int32(prediction), nil
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
