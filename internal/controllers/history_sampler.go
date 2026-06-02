package controllers

import (
	"context"
	"fmt"
	"time"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

const historySamplerTick = time.Second

// HistorySampler records model training samples on a fixed wall-clock cadence, decoupled from reconcile duration.
type HistorySampler struct {
	Controller *HPAPlusReconciler
}

func NewHistorySampler(controller *HPAPlusReconciler) *HistorySampler {
	return &HistorySampler{Controller: controller}
}

func (s *HistorySampler) NeedLeaderElection() bool {
	return true
}

func (s *HistorySampler) Start(ctx context.Context) error {
	ticker := time.NewTicker(historySamplerTick)
	defer ticker.Stop()

	s.sampleAll(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.sampleAll(ctx)
		}
	}
}

func (s *HistorySampler) sampleAll(ctx context.Context) {
	logger := ctrl.Log.WithName("history-sampler")

	instanceList := &hpaplusv1alpha1.HPAPlusList{}
	if err := s.Controller.Client.List(ctx, instanceList); err != nil {
		logger.Error(err, "failed to list HPAPlus resources for sampling")
		return
	}

	for i := range instanceList.Items {
		instance := &instanceList.Items[i]
		if err := validation.Validate(instance); err != nil {
			logger.V(1).Info("Skipping invalid HPAPlus while sampling",
				"name", instance.Name,
				"namespace", instance.Namespace,
				"error", err.Error())
			continue
		}

		if err := s.sampleInstance(ctx, instance); err != nil {
			logger.Error(err, "failed to record model history sample",
				"name", instance.Name,
				"namespace", instance.Namespace,
				"scaleTargetRef", instance.Spec.ScaleTargetRef)
		}
	}
}

func (s *HistorySampler) sampleInstance(ctx context.Context,
	instance *hpaplusv1alpha1.HPAPlus,
) error {
	now := time.Now().UTC()
	slotTime := now.Truncate(syncPeriodForInstance(instance))

	var sampleDue bool
	err := s.Controller.mutateConfigMapData(ctx, instance, func(data *hpaplusv1alpha1.HPAPlusData) (bool, error) {
		changed, due, err := s.prepareSamplingState(instance, data, now, slotTime)
		if due {
			sampleDue = true
		}
		return changed, err
	})
	if err != nil {
		return fmt.Errorf("failed to prepare sampling state: %w", err)
	}
	if !sampleDue {
		return nil
	}

	scale, _, err := s.Controller.getScaleTarget(ctx, instance)
	if err != nil {
		return fmt.Errorf("failed to get scale target: %w", err)
	}

	calculatedReplicas, cpuSnapshot, err := s.Controller.calculateReplicas(instance, scale)
	if err != nil {
		return fmt.Errorf("failed to calculate replicas for history sample: %w", err)
	}

	return s.Controller.mutateConfigMapData(ctx, instance, func(data *hpaplusv1alpha1.HPAPlusData) (bool, error) {
		return s.applySample(instance, data, now, slotTime, calculatedReplicas, cpuSnapshot)
	})
}

func (s *HistorySampler) prepareSamplingState(instance *hpaplusv1alpha1.HPAPlus,
	hpaPlusData *hpaplusv1alpha1.HPAPlusData,
	now time.Time,
	slotTime time.Time,
) (bool, bool, error) {
	changed := false
	sampleDue := false
	configuredModels := map[string]hpaplusv1alpha1.Model{}

	for _, model := range instance.Spec.Models {
		configuredModels[model.Name] = model

		modelHistory, exists := hpaPlusData.ModelHistories[model.Name]
		if !exists || modelHistory.Type != model.Type {
			modelHistory = hpaplusv1alpha1.ModelHistory{
				Type:              model.Type,
				SyncPeriodsPassed: 0,
				ReplicaHistory:    []hpaplusv1alpha1.TimestampedReplicas{},
			}
			changed = true
		}

		if model.StartInterval != nil {
			if modelHistory.StartTime == nil {
				startTime := nextInterval(now, model.StartInterval.Duration)
				modelHistory.StartTime = &metav1.Time{Time: startTime}
				hpaPlusData.ModelHistories[model.Name] = modelHistory
				changed = true
				continue
			}
			if now.Before(modelHistory.StartTime.Time) {
				hpaPlusData.ModelHistories[model.Name] = modelHistory
				continue
			}
		}

		if model.ResetDuration != nil && len(modelHistory.ReplicaHistory) > 0 {
			latest := latestTimestamp(modelHistory.ReplicaHistory)
			if latest != nil && now.Sub(*latest) > model.ResetDuration.Duration {
				modelHistory.ReplicaHistory = []hpaplusv1alpha1.TimestampedReplicas{}
				modelHistory.SyncPeriodsPassed = 0
				modelHistory.LastModelRunTime = nil
				changed = true

				if model.StartInterval != nil {
					startTime := nextInterval(now, model.StartInterval.Duration)
					modelHistory.StartTime = &metav1.Time{Time: startTime}
					hpaPlusData.ModelHistories[model.Name] = modelHistory
					continue
				}
			}
		}

		latest := latestTimestamp(modelHistory.ReplicaHistory)
		if latest == nil || slotTime.After(*latest) {
			sampleDue = true
		}
		hpaPlusData.ModelHistories[model.Name] = modelHistory
	}

	for modelName := range hpaPlusData.ModelHistories {
		if _, exists := configuredModels[modelName]; exists {
			continue
		}
		delete(hpaPlusData.ModelHistories, modelName)
		changed = true
	}

	return changed, sampleDue, nil
}

func (s *HistorySampler) applySample(instance *hpaplusv1alpha1.HPAPlus,
	hpaPlusData *hpaplusv1alpha1.HPAPlusData,
	now time.Time,
	slotTime time.Time,
	calculatedReplicas int32,
	cpuSnapshot cpuMetricSnapshot,
) (bool, error) {
	changed := false
	configuredModels := map[string]hpaplusv1alpha1.Model{}

	for _, model := range instance.Spec.Models {
		configuredModels[model.Name] = model

		modelHistory, exists := hpaPlusData.ModelHistories[model.Name]
		if !exists || modelHistory.Type != model.Type {
			modelHistory = hpaplusv1alpha1.ModelHistory{
				Type:              model.Type,
				SyncPeriodsPassed: 0,
				ReplicaHistory:    []hpaplusv1alpha1.TimestampedReplicas{},
			}
			changed = true
		}

		if model.StartInterval != nil {
			if modelHistory.StartTime == nil {
				startTime := nextInterval(now, model.StartInterval.Duration)
				modelHistory.StartTime = &metav1.Time{Time: startTime}
				hpaPlusData.ModelHistories[model.Name] = modelHistory
				changed = true
				continue
			}
			if now.Before(modelHistory.StartTime.Time) {
				hpaPlusData.ModelHistories[model.Name] = modelHistory
				continue
			}
		}

		if model.ResetDuration != nil && len(modelHistory.ReplicaHistory) > 0 {
			latest := latestTimestamp(modelHistory.ReplicaHistory)
			if latest != nil && now.Sub(*latest) > model.ResetDuration.Duration {
				modelHistory.ReplicaHistory = []hpaplusv1alpha1.TimestampedReplicas{}
				modelHistory.SyncPeriodsPassed = 0
				modelHistory.LastModelRunTime = nil
				changed = true

				if model.StartInterval != nil {
					startTime := nextInterval(now, model.StartInterval.Duration)
					modelHistory.StartTime = &metav1.Time{Time: startTime}
					hpaPlusData.ModelHistories[model.Name] = modelHistory
					continue
				}
			}
		}

		latest := latestTimestamp(modelHistory.ReplicaHistory)
		if latest != nil && !slotTime.After(*latest) {
			hpaPlusData.ModelHistories[model.Name] = modelHistory
			continue
		}

		modelHistory.ReplicaHistory = append(modelHistory.ReplicaHistory, hpaplusv1alpha1.TimestampedReplicas{
			Time:                           &metav1.Time{Time: slotTime},
			Replicas:                       calculatedReplicas,
			Metric:                         cpuSnapshot.Utilization,
			TotalCPUUsageMillicores:        cpuSnapshot.TotalUsageMillicores,
			RequestPerPodMillicores:        cpuSnapshot.RequestPerPodMillicores,
			TargetCPUUtilizationPercentage: cpuSnapshot.TargetUtilizationPercentage,
		})

		prunedHistory, err := s.Controller.Predicter.PruneHistory(&model, modelHistory.ReplicaHistory)
		if err != nil {
			return false, fmt.Errorf("failed to prune model history for %s: %w", model.Name, err)
		}

		modelHistory.ReplicaHistory = prunedHistory
		hpaPlusData.ModelHistories[model.Name] = modelHistory
		changed = true
	}

	for modelName := range hpaPlusData.ModelHistories {
		if _, exists := configuredModels[modelName]; exists {
			continue
		}
		delete(hpaPlusData.ModelHistories, modelName)
		changed = true
	}

	return changed, nil
}
