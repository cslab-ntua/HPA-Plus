package validation

import (
	"fmt"
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/workloaddemand"
)

func validateRequestDemand(spec api.HPAPlusSpec) error {
	if spec.PredictionFailurePolicy != "" && spec.PredictionFailurePolicy != api.PredictionFailureHoldCurrent && spec.PredictionFailurePolicy != api.PredictionFailureReactive {
		return fmt.Errorf("predictionFailurePolicy must be HoldCurrent or Reactive")
	}
	for _, model := range spec.Models {
		if model.RequestDemand == nil {
			continue
		}
		if spec.SyncPeriod == nil || *spec.SyncPeriod != 30000 {
			return fmt.Errorf("model %q: requestDemand requires an explicit 30000 millisecond syncPeriod", model.Name)
		}
		if model.Type != api.TypeXGBoost || model.XGBoost == nil {
			return fmt.Errorf("model %q: requestDemand is supported only by XGBoost", model.Name)
		}
		if model.XGBoost.ForecastStrategy != "direct" {
			return fmt.Errorf("model %q: requestDemand requires XGBoost direct forecast strategy", model.Name)
		}
		if model.PredictivePolicy == nil || model.PredictionMaxAge == nil {
			return fmt.Errorf("model %q: requestDemand requires predictivePolicy and predictionMaxAge", model.Name)
		}
		if spec.PredictionFailurePolicy != api.PredictionFailureHoldCurrent {
			return fmt.Errorf("requestDemand requires predictionFailurePolicy HoldCurrent")
		}
		if spec.IncludeHPA == nil || *spec.IncludeHPA || spec.EffectiveMode() == api.ModeAssist {
			return fmt.Errorf("requestDemand requires explicit includeHPA false and control or observe mode")
		}
		if err := workloaddemand.ValidateConfig(*model.RequestDemand); err != nil {
			return fmt.Errorf("model %q: %w", model.Name, err)
		}
	}
	return nil
}
