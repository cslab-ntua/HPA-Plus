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

	autoscalingv2 "k8s.io/api/autoscaling/v2"

	jamiethompsonmev1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

// Validate performs validation on the HPA+, will return an error if the HPA+ is not valid
func Validate(instance *jamiethompsonmev1alpha1.PredictiveHorizontalPodAutoscaler) error {
	spec := instance.Spec

	err := validateMinMax(spec)
	if err != nil {
		return err
	}

	err = validateModels(spec)
	if err != nil {
		return err
	}

	return nil
}

func validateMinMax(spec jamiethompsonmev1alpha1.PredictiveHorizontalPodAutoscalerSpec) error {
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

func validateModels(spec jamiethompsonmev1alpha1.PredictiveHorizontalPodAutoscalerSpec) error {
	hasCPUUtilizationMetric := false
	for _, metric := range spec.Metrics {
		if metric.Type == autoscalingv2.ResourceMetricSourceType &&
			metric.Resource != nil &&
			metric.Resource.Name == "cpu" &&
			metric.Resource.Target.AverageUtilization != nil {
			hasCPUUtilizationMetric = true
		}
	}

	for _, model := range spec.Models {
		if model.Type == jamiethompsonmev1alpha1.TypeHoltWinters {
			hw := model.HoltWinters
			if hw == nil {
				return fmt.Errorf("invalid model '%s', type is '%s' but no Holt Winters configuration provided",
					model.Name, model.Type)
			}

			if hw.RuntimeTuningFetchHook != nil {
				hook := hw.RuntimeTuningFetchHook
				if hook.Type == jamiethompsonmev1alpha1.HookTypeHTTP && hook.HTTP == nil {
					return fmt.Errorf("invalid model '%s', runtimeTuningFetchHook is type '%s' but no HTTP hook configuration provided",
						model.Name, hook.Type)
				}
			}
		}

		if model.Type == jamiethompsonmev1alpha1.TypeLinear && model.Linear == nil {
			return fmt.Errorf("invalid model '%s', type is '%s' but no Linear Regression configuration provided",
				model.Name, model.Type)
		}

		if model.Type == jamiethompsonmev1alpha1.TypeArima && model.Arima == nil {
			return fmt.Errorf("invalid model '%s', type is '%s' but no ARIMA configuration provided",
				model.Name, model.Type)
		}

		if model.Type == jamiethompsonmev1alpha1.TypeArima && model.Arima != nil {
			if !hasCPUUtilizationMetric {
				return fmt.Errorf("invalid model '%s', ARIMA CPU-history prediction requires a CPU resource metric with averageUtilization configured", model.Name)
			}
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
			if arima.UseSarima != nil && *arima.UseSarima {
				if arima.SeasonalOrder == nil || len(arima.SeasonalOrder) != 3 {
					return fmt.Errorf("invalid model '%s', SARIMA enabled but seasonalOrder is missing or invalid", model.Name)
				}
				if arima.SeasonalPeriods == nil || *arima.SeasonalPeriods <= 0 {
					return fmt.Errorf("invalid model '%s', SARIMA enabled but seasonalPeriods is missing or invalid", model.Name)
				}
			}
			if arima.RefitEvery != nil && *arima.RefitEvery <= 0 {
				return fmt.Errorf("invalid model '%s', ARIMA refitEvery must be greater than 0 when provided", model.Name)
			}
		}

		if model.Type == jamiethompsonmev1alpha1.TypeXGBoost {
			xb := model.XGBoost
			if xb == nil {
				return fmt.Errorf("invalid model '%s', type is '%s' but no XGBoost configuration provided",
					model.Name, model.Type)
			}
			if !hasCPUUtilizationMetric {
				return fmt.Errorf("invalid model '%s', XGBoost CPU-history prediction requires a CPU resource metric with averageUtilization configured", model.Name)
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
			if xb.Lags > xb.HistorySize {
				return fmt.Errorf("invalid model '%s', XGBoost lags (%d) cannot exceed historySize (%d)",
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

		if model.Type == jamiethompsonmev1alpha1.TypeLightGBM {
			lgb := model.LightGBM
			if lgb == nil {
				return fmt.Errorf("invalid model '%s', type is '%s' but no LightGBM configuration provided",
					model.Name, model.Type)
			}
			if !hasCPUUtilizationMetric {
				return fmt.Errorf("invalid model '%s', LightGBM CPU-history prediction requires a CPU resource metric with averageUtilization configured", model.Name)
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
			if lgb.Lags > lgb.HistorySize {
				return fmt.Errorf("invalid model '%s', LightGBM lags (%d) cannot exceed historySize (%d)",
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
	}
	return nil
}
