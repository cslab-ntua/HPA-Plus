/*
Copyright 2023 The Predictive Horizontal Pod Autoscaler Authors.

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

package validation

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
)

// Validate performs validation on the HPA+, will return an error if the HPA+ is not valid
func Validate(instance *hpaplusv1alpha1.HPAPlus) error {
	if instance == nil {
		return errors.New("HPAPlus must not be nil")
	}
	spec := instance.Spec
	if err := validateSpec(spec); err != nil {
		return err
	}

	err := validateMinMax(spec)
	if err != nil {
		return err
	}

	err = validateModels(spec)
	if err != nil {
		return err
	}

	return validateKubernetesSpec(spec)
}

func validateMinMax(spec hpaplusv1alpha1.HPAPlusSpec) error {
	if spec.MinReplicas != nil && spec.MaxReplicas < *spec.MinReplicas {
		return fmt.Errorf("spec.maxReplicas (%d) cannot be less than spec.minReplicas (%d)",
			spec.MaxReplicas, *spec.MinReplicas)
	}

	if spec.MinReplicas != nil && *spec.MinReplicas == 0 {
		// We need to check that if they set min replicas to zero they have at least 1 object or external metric
		// configured
		valid := false
		for _, metric := range spec.Metrics {
			if metric.Type == autoscalingv2.ObjectMetricSourceType || metric.Type == autoscalingv2.ExternalMetricSourceType {
				valid = true
				break
			}
		}
		if !valid {
			return errors.New("spec.minReplicas can only be 0 if you have at least 1 object or external metric configured")
		}
	}
	return nil
}

func validateModels(spec hpaplusv1alpha1.HPAPlusSpec) error {
	hasCPUUtilizationMetric := false
	for _, metric := range spec.Metrics {
		if metric.Type == autoscalingv2.ResourceMetricSourceType &&
			metric.Resource != nil &&
			metric.Resource.Name == "cpu" &&
			metric.Resource.Target.Type == autoscalingv2.UtilizationMetricType &&
			metric.Resource.Target.AverageUtilization != nil && *metric.Resource.Target.AverageUtilization > 0 {
			hasCPUUtilizationMetric = true
		}
	}

	names := map[string]bool{}
	for _, model := range spec.Models {
		if strings.TrimSpace(model.Name) == "" || names[model.Name] {
			return fmt.Errorf("model names must be non-empty and unique: %q", model.Name)
		}
		names[model.Name] = true
		if err := validateModelBasics(model); err != nil {
			return fmt.Errorf("invalid model '%s': %w", model.Name, err)
		}
		if model.Type == hpaplusv1alpha1.TypeHoltWinters {
			hw := model.HoltWinters
			if hw == nil {
				return fmt.Errorf("invalid model '%s', type is '%s' but no Holt Winters configuration provided",
					model.Name, model.Type)
			}
			if !hasCPUUtilizationMetric {
				return fmt.Errorf("invalid model '%s', Holt-Winters CPU-history prediction requires a CPU resource metric with averageUtilization configured", model.Name)
			}

			if hw.LookAhead != nil && !validDuration(*hw.LookAhead, time.Millisecond, 1) {
				return fmt.Errorf("invalid model %q: Holt-Winters lookAhead must be a positive millisecond duration", model.Name)
			}
			if hw.RuntimeTuningFetchHook != nil {
				if err := validateHook(hw.RuntimeTuningFetchHook); err != nil {
					return fmt.Errorf("invalid model %q: %w", model.Name, err)
				}
			}
			if err := validateInitialization(hw); err != nil {
				return fmt.Errorf("invalid model %q: %w", model.Name, err)
			}
		}

		if model.Type == hpaplusv1alpha1.TypeLinear {
			if model.Linear == nil {
				return fmt.Errorf("invalid model '%s', type is '%s' but no Linear Regression configuration provided",
					model.Name, model.Type)
			}
			if !hasCPUUtilizationMetric {
				return fmt.Errorf("invalid model '%s', Linear CPU-history prediction requires a CPU resource metric with averageUtilization configured", model.Name)
			}
			if model.Linear.HistorySize < 2 || model.Linear.LookAhead < 1 {
				return fmt.Errorf("invalid model '%s', Linear historySize must be >= 2 and lookAhead must be >= 1", model.Name)
			}
		}

		if model.Type == hpaplusv1alpha1.TypeArima && model.Arima == nil {
			return fmt.Errorf("invalid model '%s', type is '%s' but no ARIMA configuration provided",
				model.Name, model.Type)
		}

		if model.Type == hpaplusv1alpha1.TypeArima && model.Arima != nil {
			arima := model.Arima
			if len(arima.Order) != 3 {
				return fmt.Errorf("invalid model '%s', ARIMA order must have exactly 3 parameters [p, d, q], got %d",
					model.Name, len(arima.Order))
			}
			for i, param := range arima.Order {
				if param < 0 {
					return fmt.Errorf("invalid model '%s', ARIMA order parameter %d must be non-negative, got %d",
						model.Name, i, param)
				}
			}
			if arima.InformationCriterion != nil && (*arima.InformationCriterion != "aic" && *arima.InformationCriterion != "bic") {
				return fmt.Errorf("invalid model '%s', ARIMA information criterion must be 'aic' or 'bic', got '%s'",
					model.Name, *arima.InformationCriterion)
			}
			if arima.SeasonalOrder != nil && len(arima.SeasonalOrder) != 0 && len(arima.SeasonalOrder) != 3 {
				return fmt.Errorf("invalid model '%s', ARIMA seasonal order must have exactly 3 parameters [P, D, Q], got %d",
					model.Name, len(arima.SeasonalOrder))
			}
			for i, param := range arima.SeasonalOrder {
				if param < 0 {
					return fmt.Errorf("invalid model '%s', ARIMA seasonal order parameter %d must be non-negative, got %d",
						model.Name, i, param)
				}
			}
			if (arima.UseSarima != nil && *arima.UseSarima) || arima.SeasonalOrder != nil || arima.SeasonalPeriods != nil {
				if arima.SeasonalOrder == nil || len(arima.SeasonalOrder) != 3 {
					return fmt.Errorf("invalid model '%s', SARIMA enabled but seasonalOrder is missing or invalid", model.Name)
				}
				if arima.SeasonalPeriods == nil || *arima.SeasonalPeriods < 2 {
					return fmt.Errorf("invalid model '%s', SARIMA enabled but seasonalPeriods is missing or invalid", model.Name)
				}
			}
			if arima.LookAhead < 1 || (arima.HistorySize != nil && *arima.HistorySize < 3) {
				return fmt.Errorf("invalid model '%s', ARIMA requires lookAhead >= 1 and historySize >= 3", model.Name)
			}
			if arima.MaxOrder != nil {
				if len(arima.MaxOrder) != 3 {
					return fmt.Errorf("invalid model '%s', ARIMA maxOrder must contain 3 non-negative parameters", model.Name)
				}
				for _, value := range arima.MaxOrder {
					if value < 0 {
						return fmt.Errorf("invalid model '%s', ARIMA maxOrder must contain 3 non-negative parameters", model.Name)
					}
				}
			}
			if arima.Trend != nil && !oneOf(*arima.Trend, "n", "c", "t", "ct") {
				return fmt.Errorf("invalid model '%s', ARIMA trend must be n, c, t, or ct", model.Name)
			}
			if arima.AutoArima != nil && *arima.AutoArima && arima.IncrementalUpdates != nil && *arima.IncrementalUpdates {
				return fmt.Errorf("invalid model '%s', autoArima is not supported with incrementalUpdates", model.Name)
			}
			seasonal := (arima.UseSarima != nil && *arima.UseSarima) || arima.SeasonalOrder != nil || arima.SeasonalPeriods != nil
			auto := arima.AutoArima != nil && *arima.AutoArima
			if auto && seasonal {
				return fmt.Errorf("invalid model %q: autoArima supports non-seasonal ARIMA only", model.Name)
			}
			if auto && arima.MaxOrder != nil {
				for i, value := range arima.Order {
					if value > arima.MaxOrder[i] {
						return fmt.Errorf("ARIMA order must not exceed explicit maxOrder during auto selection")
					}
				}
			}
			if !seasonal && !auto && arima.Trend != nil {
				trend, d := *arima.Trend, arima.Order[1]
				if d > 0 && (trend == "c" || trend == "ct") || d > 1 && trend == "t" {
					return fmt.Errorf("ARIMA trend degree must be at least the differencing order")
				}
			}
			if arima.RefitEvery != nil && *arima.RefitEvery <= 0 {
				return fmt.Errorf("invalid model '%s', ARIMA refitEvery must be greater than 0 when provided", model.Name)
			}
		}

		if model.Type == hpaplusv1alpha1.TypeXGBoost {
			xb := model.XGBoost
			if xb == nil {
				return fmt.Errorf("invalid model '%s', type is '%s' but no XGBoost configuration provided",
					model.Name, model.Type)
			}
			if xb.HistorySize < 1 {
				return fmt.Errorf("invalid model '%s', XGBoost historySize must be >= 1", model.Name)
			}
			if xb.LookAhead < 1 {
				return fmt.Errorf("invalid model '%s', XGBoost lookAhead must be >= 1", model.Name)
			}
			if xb.Lags < 1 {
				return fmt.Errorf("invalid model '%s', XGBoost lags must be >= 1", model.Name)
			}
			if xb.Lags >= xb.HistorySize {
				return fmt.Errorf("invalid model '%s', XGBoost lags (%d) must be less than historySize (%d)",
					model.Name, xb.Lags, xb.HistorySize)
			}
			if xb.WindowSize != nil && *xb.WindowSize < 1 {
				return fmt.Errorf("invalid model '%s', XGBoost windowSize must be >= 1", model.Name)
			}
			if xb.WindowSize != nil && *xb.WindowSize > xb.Lags {
				return fmt.Errorf("invalid model '%s', XGBoost windowSize (%d) cannot exceed lags (%d)",
					model.Name, *xb.WindowSize, xb.Lags)
			}
			if xb.MaxDepth != nil && *xb.MaxDepth < 1 {
				return fmt.Errorf("invalid model '%s', XGBoost maxDepth must be >= 1", model.Name)
			}
			if xb.NEstimators != nil && *xb.NEstimators < 1 {
				return fmt.Errorf("invalid model '%s', XGBoost nEstimators must be >= 1", model.Name)
			}
			if xb.LearningRate != nil && *xb.LearningRate <= 0 {
				return fmt.Errorf("invalid model '%s', XGBoost learningRate must be > 0", model.Name)
			}
			if xb.Subsample != nil && (*xb.Subsample <= 0 || *xb.Subsample > 1) {
				return fmt.Errorf("invalid model '%s', XGBoost subsample must be in (0, 1]", model.Name)
			}
			if xb.ColsampleBytree != nil && (*xb.ColsampleBytree <= 0 || *xb.ColsampleBytree > 1) {
				return fmt.Errorf("invalid model '%s', XGBoost colsampleBytree must be in (0, 1]", model.Name)
			}
			if xb.RegLambda != nil && *xb.RegLambda < 0 {
				return fmt.Errorf("invalid model '%s', XGBoost regLambda must be >= 0", model.Name)
			}
			if xb.RegAlpha != nil && *xb.RegAlpha < 0 {
				return fmt.Errorf("invalid model '%s', XGBoost regAlpha must be >= 0", model.Name)
			}
		}

		if model.Type == hpaplusv1alpha1.TypeLightGBM {
			lgb := model.LightGBM
			if lgb == nil {
				return fmt.Errorf("invalid model '%s', type is '%s' but no LightGBM configuration provided",
					model.Name, model.Type)
			}
			if lgb.HistorySize < 1 {
				return fmt.Errorf("invalid model '%s', LightGBM historySize must be >= 1", model.Name)
			}
			if lgb.LookAhead < 1 {
				return fmt.Errorf("invalid model '%s', LightGBM lookAhead must be >= 1", model.Name)
			}
			if lgb.Lags < 1 {
				return fmt.Errorf("invalid model '%s', LightGBM lags must be >= 1", model.Name)
			}
			if lgb.Lags >= lgb.HistorySize {
				return fmt.Errorf("invalid model '%s', LightGBM lags (%d) must be less than historySize (%d)",
					model.Name, lgb.Lags, lgb.HistorySize)
			}
			if lgb.WindowSize != nil && *lgb.WindowSize < 1 {
				return fmt.Errorf("invalid model '%s', LightGBM windowSize must be >= 1", model.Name)
			}
			if lgb.WindowSize != nil && *lgb.WindowSize > lgb.Lags {
				return fmt.Errorf("invalid model '%s', LightGBM windowSize (%d) cannot exceed lags (%d)",
					model.Name, *lgb.WindowSize, lgb.Lags)
			}
			if lgb.MaxDepth != nil && *lgb.MaxDepth != -1 && *lgb.MaxDepth < 1 {
				return fmt.Errorf("invalid model '%s', LightGBM maxDepth must be -1 or >= 1", model.Name)
			}
			if lgb.NEstimators != nil && *lgb.NEstimators < 1 {
				return fmt.Errorf("invalid model '%s', LightGBM nEstimators must be >= 1", model.Name)
			}
			if lgb.LearningRate != nil && *lgb.LearningRate <= 0 {
				return fmt.Errorf("invalid model '%s', LightGBM learningRate must be > 0", model.Name)
			}
			if lgb.Subsample != nil && (*lgb.Subsample <= 0 || *lgb.Subsample > 1) {
				return fmt.Errorf("invalid model '%s', LightGBM subsample must be in (0, 1]", model.Name)
			}
			if lgb.ColsampleBytree != nil && (*lgb.ColsampleBytree <= 0 || *lgb.ColsampleBytree > 1) {
				return fmt.Errorf("invalid model '%s', LightGBM colsampleBytree must be in (0, 1]", model.Name)
			}
			if lgb.NumLeaves != nil && *lgb.NumLeaves < 2 {
				return fmt.Errorf("invalid model '%s', LightGBM numLeaves must be >= 2", model.Name)
			}
			if lgb.MinChildSamples != nil && *lgb.MinChildSamples < 1 {
				return fmt.Errorf("invalid model '%s', LightGBM minChildSamples must be >= 1", model.Name)
			}
			if lgb.RegLambda != nil && *lgb.RegLambda < 0 {
				return fmt.Errorf("invalid model '%s', LightGBM regLambda must be >= 0", model.Name)
			}
			if lgb.RegAlpha != nil && *lgb.RegAlpha < 0 {
				return fmt.Errorf("invalid model '%s', LightGBM regAlpha must be >= 0", model.Name)
			}
		}

		if prediction.UsesCPUHistory(model.Type) && !hasCPUUtilizationMetric {
			return fmt.Errorf("invalid model '%s', %s CPU-history prediction requires a CPU resource metric with averageUtilization configured", model.Name, model.Type)
		}
		if err := validateModelHistory(model, spec.SyncPeriod); err != nil {
			return fmt.Errorf("invalid model '%s': %w", model.Name, err)
		}
	}
	return nil
}

func validateSpec(spec hpaplusv1alpha1.HPAPlusSpec) error {
	switch spec.EffectiveMode() {
	case hpaplusv1alpha1.ModeObserve, hpaplusv1alpha1.ModeAssist, hpaplusv1alpha1.ModeControl:
	default:
		return fmt.Errorf("spec.mode must be observe, assist or control")
	}

	if spec.MaxReplicas < 1 || (spec.MinReplicas != nil && *spec.MinReplicas < 0) {
		return errors.New("maxReplicas must be >= 1 and minReplicas must be >= 0")
	}
	if spec.Tolerance != nil && !finiteNonnegative(*spec.Tolerance) {
		return errors.New("tolerance must be finite and >= 0")
	}
	if spec.SyncPeriod != nil && !validDuration(*spec.SyncPeriod, time.Millisecond, 1) {
		return errors.New("syncPeriod must be a positive representable duration in milliseconds")
	}
	if spec.MetricMaxAge != nil && !validDuration(*spec.MetricMaxAge, time.Millisecond, 1) {
		return errors.New("metricMaxAge must be a positive representable duration in milliseconds")
	}
	for _, value := range []*int{spec.CPUInitializationPeriod, spec.InitialReadinessDelay} {
		if value != nil && !validDuration(*value, time.Second, 0) {
			return errors.New("CPU initialization and readiness delays must be non-negative representable durations in seconds")
		}
	}
	if spec.DecisionType != nil && !oneOf(*spec.DecisionType, "maximum", "minimum", "mean", "median") {
		return errors.New("decisionType must be maximum, minimum, mean, or median")
	}
	return nil
}

func validateModelBasics(model hpaplusv1alpha1.Model) error {
	if model.ForecastReliability != nil {
		p := model.ForecastReliability.Effective()
		if p.CalibrationSamples < 10 || p.CalibrationSamples > 100 || p.Window < 2 || p.Window > 20 || p.Breaches < 2 || p.Breaches > p.Window || p.Breaches*2 <= p.Window || math.IsNaN(p.MinimumRelativeError) || math.IsInf(p.MinimumRelativeError, 0) || p.MinimumRelativeError < .001 || p.MinimumRelativeError > 1 {
			return errors.New("forecastReliability requires 10..100 calibration samples, a 2..20 window, a strict majority of at least two breaches, and minimumRelativeError in [0.001, 1]")
		}
	}

	if p := model.PredictivePolicy; p != nil {
		if !prediction.UsesCPUHistory(model.Type) || model.PredictionMaxAge == nil {
			return errors.New("predictivePolicy requires a supported CPU forecasting model and predictionMaxAge")
		}
		tolerance := p.EffectiveTolerance()
		if p.ScaleUpLeadSeconds < 0 || p.ScaleUpLeadSeconds > 3600 || math.IsNaN(tolerance) || math.IsInf(tolerance, 0) || tolerance < 0 || tolerance > .2 {
			return errors.New("predictivePolicy requires lead time 0..3600 seconds and finite tolerance 0..0.2")
		}
		if p.ScaleUpLeadFromStartup && model.RequestDemand == nil {
			return errors.New("predictivePolicy.scaleUpLeadFromStartup requires requestDemand")
		}
	}

	if !prediction.UsesCPUHistory(model.Type) {
		return fmt.Errorf("unknown model type %q", model.Type)
	}
	configs := 0
	for _, present := range []bool{model.Linear != nil, model.HoltWinters != nil, model.Arima != nil, model.XGBoost != nil, model.LightGBM != nil} {
		if present {
			configs++
		}
	}
	if configs > 1 {
		return errors.New("only one model configuration block is allowed")
	}
	if model.CalculationTimeout != nil && !validDuration(*model.CalculationTimeout, time.Millisecond, 1) {
		return errors.New("calculationTimeout must be a positive representable duration in milliseconds")
	}
	if model.PerSyncPeriod != nil && *model.PerSyncPeriod < 1 {
		return errors.New("perSyncPeriod must be >= 1")
	}
	if model.PredictionMaxAge != nil && !validDuration(*model.PredictionMaxAge, time.Millisecond, 1) {
		return errors.New("predictionMaxAge must be a positive representable duration in milliseconds")
	}
	if model.HistoryMaxAge != nil && !validDuration(*model.HistoryMaxAge, time.Millisecond, 1) {
		return errors.New("historyMaxAge must be a positive representable duration in milliseconds")
	}
	if model.StartInterval != nil && model.StartInterval.Duration <= 0 {
		return errors.New("startInterval must be positive")
	}
	if model.ResetDuration != nil && model.ResetDuration.Duration <= 0 {
		return errors.New("resetDuration must be positive")
	}
	var numeric []*float64
	if model.XGBoost != nil {
		x := model.XGBoost
		if power := x.CPUDemandWeightExponent; power != nil && (math.IsNaN(*power) || math.IsInf(*power, 0) || *power < 0 || *power > 2) {
			return errors.New("cpuDemandWeightExponent must be finite and in [0, 2]")
		}
		if !oneOf(x.ForecastStrategy, "", "recursive", "direct") {
			return errors.New("XGBoost forecastStrategy must be recursive or direct")
		}
		if !oneOf(x.ForecastAggregation, "", "last", "maximum") {
			return errors.New("XGBoost forecastAggregation must be last or maximum")
		}
		if x.Objective != nil && !oneOf(*x.Objective, "reg:squarederror", "reg:squaredlogerror", "reg:absoluteerror", "reg:pseudohubererror", "count:poisson", "reg:gamma", "reg:tweedie", "reg:quantileerror", "survival:aft") {
			return errors.New("XGBoost objective must predict CPU demand using a supported regression objective")
		}
		quantile := x.Objective != nil && *x.Objective == "reg:quantileerror"
		aft := x.Objective != nil && *x.Objective == "survival:aft"
		if aft {
			if x.UseUtilizationFeatures != nil && !*x.UseUtilizationFeatures {
				return errors.New("survival:aft requires utilization features")
			}
			if x.ForecastStrategy != "direct" || x.AFTLossDistributionScale == nil || x.CPUCensoringUtilizationThreshold == nil {
				return errors.New("survival:aft requires direct forecasting, aftLossDistributionScale and cpuCensoringUtilizationThreshold")
			}
			if v := *x.AFTLossDistributionScale; math.IsNaN(v) || math.IsInf(v, 0) || v < .05 || v > 2 {
				return errors.New("aftLossDistributionScale must be finite and in [0.05, 2]")
			}
			if v := *x.CPUCensoringUtilizationThreshold; math.IsNaN(v) || math.IsInf(v, 0) || v < 90 || v > 10000 {
				return errors.New("cpuCensoringUtilizationThreshold must be finite and in [90, 10000]")
			}
		} else if x.AFTLossDistributionScale != nil || x.CPUCensoringUtilizationThreshold != nil {
			return errors.New("AFT parameters require objective survival:aft")
		}
		if quantile != (x.QuantileAlpha != nil) || (x.QuantileAlpha != nil && (math.IsNaN(*x.QuantileAlpha) || math.IsInf(*x.QuantileAlpha, 0) || *x.QuantileAlpha <= 0 || *x.QuantileAlpha >= 1)) {
			return errors.New("XGBoost reg:quantileerror requires one finite quantileAlpha strictly between zero and one; other objectives must omit it")
		}
		numeric = []*float64{x.LearningRate, x.Subsample, x.ColsampleBytree, x.MinChildWeight, x.Gamma, x.RegLambda, x.RegAlpha}
	}
	if model.LightGBM != nil {
		x := model.LightGBM
		if x.Objective != nil && !oneOf(*x.Objective, "regression", "regression_l2", "l2", "mean_squared_error", "mse", "regression_l1", "l1", "mean_absolute_error", "mae", "huber", "fair", "poisson", "quantile", "mape", "mean_absolute_percentage_error", "gamma", "tweedie") {
			return errors.New("LightGBM objective must predict CPU demand using a supported regression objective")
		}
		numeric = []*float64{x.LearningRate, x.Subsample, x.ColsampleBytree, x.RegLambda, x.RegAlpha}
	}
	for _, value := range numeric {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return errors.New("model numeric parameters must be finite")
		}
	}
	if model.XGBoost != nil {
		for _, value := range []*float64{model.XGBoost.MinChildWeight, model.XGBoost.Gamma} {
			if value != nil && *value < 0 {
				return errors.New("XGBoost minChildWeight and gamma must be >= 0")
			}
		}
	}
	return nil
}

// A model must retain enough samples both to fit and to reach its run cadence.
// Otherwise pruning can prevent it from ever becoming eligible again.
func validateModelHistory(model hpaplusv1alpha1.Model, syncPeriod *int) error {
	if model.PredictivePolicy != nil && model.PredictionMaxAge != nil {
		cadence := hpaplusv1alpha1.DefaultSyncPeriodMillis
		if syncPeriod != nil {
			cadence = *syncPeriod
		}
		if *model.PredictionMaxAge <= cadence {
			return errors.New("timed predictionMaxAge must exceed the sampling cadence so a prediction remains usable at its timestamp")
		}
	}

	retained := 0
	switch model.Type {
	case hpaplusv1alpha1.TypeLinear:
		retained = model.Linear.HistorySize
	case hpaplusv1alpha1.TypeXGBoost:
		retained = model.XGBoost.HistorySize
	case hpaplusv1alpha1.TypeLightGBM:
		retained = model.LightGBM.HistorySize
	case hpaplusv1alpha1.TypeArima:
		retained = hpaplusv1alpha1.DefaultARIMAHistorySize
		if model.Arima.HistorySize != nil {
			retained = *model.Arima.HistorySize
		}
	case hpaplusv1alpha1.TypeHoltWinters:
		hw := model.HoltWinters
		if hw.SeasonalPeriods < 2 || hw.StoredSeasons < 1 || hw.SeasonalPeriods > math.MaxInt/2-10 || hw.StoredSeasons > math.MaxInt/hw.SeasonalPeriods {
			return errors.New("Holt-Winters requires seasonalPeriods >= 2 and positive, non-overflowing storedSeasons")
		}
		retained = hw.SeasonalPeriods * hw.StoredSeasons
		minimum := 2 * hw.SeasonalPeriods
		if heuristic := 10 + 2*(hw.SeasonalPeriods/2); heuristic > minimum {
			minimum = heuristic
		}
		if retained < minimum {
			return fmt.Errorf("Holt-Winters retained history (%d) must be >= initialization minimum (%d)", retained, minimum)
		}
		if !oneOf(hw.Trend, "add", "additive", "mul", "multiplicative") || !oneOf(hw.Seasonal, "add", "additive", "mul", "multiplicative") {
			return errors.New("Holt-Winters trend and seasonal must be add/additive or mul/multiplicative")
		}
		for _, value := range []*float64{hw.Alpha, hw.Beta, hw.Gamma} {
			if value == nil {
				if hw.RuntimeTuningFetchHook == nil {
					return errors.New("Holt-Winters requires alpha, beta, and gamma or a runtime tuning hook")
				}
			} else if !finiteNonnegative(*value) || *value > 1 {
				return errors.New("Holt-Winters alpha, beta, and gamma must be finite and in [0, 1]")
			}
		}
	}
	if model.PerSyncPeriod != nil && *model.PerSyncPeriod > retained {
		return fmt.Errorf("perSyncPeriod (%d) cannot exceed retained history (%d)", *model.PerSyncPeriod, retained)
	}
	if model.HistoryMaxAge != nil {
		period := int64(hpaplusv1alpha1.DefaultSyncPeriodMillis) // Default sampler cadence, in milliseconds.
		if syncPeriod != nil {
			period = int64(*syncPeriod)
		}
		if period < 1000 {
			period = 1000 // The sampler wakes at most once per second.
		}
		// Whole-second persistence rounds fractional slots. Reserve one full
		// rounded interval per required sample, including room before the next tick.
		period = ((period + 999) / 1000) * 1000
		if int64(*model.HistoryMaxAge)/period < int64(retained) {
			return fmt.Errorf("historyMaxAge must retain %d samples at a nominal %d ms sampling interval", retained, period)
		}
	}
	return nil
}

func validDuration(value int, unit time.Duration, minimum int) bool {
	return value >= minimum && int64(value) <= math.MaxInt64/int64(unit)
}

func finiteNonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}
