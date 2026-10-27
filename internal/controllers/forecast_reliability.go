package controllers

import (
	"context"
	"errors"
	"reflect"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/forecastquality"
)

var errReliabilityIdentityChanged = errors.New("forecast reliability identity changed")

func (r *HPAPlusReconciler) evaluateForecastReliability(ctx context.Context, instance *api.HPAPlus, model *api.Model, epoch int64, observation cpuMetricSnapshot, fingerprint string) (api.ModelHistory, error) {
	var updated api.ModelHistory
	err := r.mutateConfigMapData(ctx, instance, func(data *api.HPAPlusData) (bool, error) {
		current, ok := data.ModelHistories[model.Name]
		if !ok || current.Type != model.Type || current.HistoryEpoch != epoch || data.TargetIdentity != observation.TargetIdentity {
			return false, errReliabilityIdentityChanged
		}
		before := current.Reliability
		current.Reliability = forecastquality.Ensure(before, fingerprint)
		now := r.now()
		if !observation.MetricsIncomplete && observation.SourceID != "" && observation.TotalUsageMillicores != nil && observation.MetricsValidUntil != nil && now.Before(*observation.MetricsValidUntil) {
			forecastquality.Observe(current.Reliability, *model.ForecastReliability, observation.SourceTime, *observation.TotalUsageMillicores, now)
		}
		updated = current
		if reflect.DeepEqual(before, current.Reliability) {
			return false, nil
		}
		data.ModelHistories[model.Name] = current
		return true, nil
	})
	return updated, err
}
