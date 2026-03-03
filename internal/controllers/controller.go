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

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/jthomperoo/k8shorizmetrics/v2/metricsclient"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/scale"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	jamiethompsonmev1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	"github.com/cslab-ntua/HPA-Plus/internal/scalebehavior"
	"github.com/cslab-ntua/HPA-Plus/internal/validation"
	"github.com/jthomperoo/k8shorizmetrics/v2"
)

// HPA+ configuration constants
const (
	defaultSyncPeriod       = 15 * time.Second
	defaultErrorRetryPeriod = 10 * time.Second
)

// HPA calculation configuration constants
const (
	defaultCPUInitializationPeriod = 300
	defaultInitialReadinessDelay   = 30
	defaultTolerance               = 0.1
	defaultPerSyncPeriod           = 1
)

// HPA+ scale constraints
const (
	defaultDecisionType = jamiethompsonmev1alpha1.DecisionMaximum
	defaultMinReplicas  = 1
)

// Downscale constants
const (
	defaultDownscaleStabilization                 = int32(300)
	defaultDownscalePercentagePolicyPeriodSeconds = int32(60)
	defaultDownscalePercentagePolicyValue         = int32(100)
)

// Upscale constants
const (
	defaultUpscaleStabilization                 = int32(0)
	defaultUpscalePercentagePolicyPeriodSeconds = int32(60)
	defaultUpscalePercentagePolicyValue         = int32(100)
	defaultUpscalePodsPolicyPeriodSeconds       = int32(60)
	defaultUpscalePodsPolicyValue               = int32(4)
)

const (
	configMapDataKey        = "data"
	defaultArimaHistorySize = 50
)

// PredictiveHorizontalPodAutoscalerReconciler reconciles a PredictiveHorizontalPodAutoscaler object
type PredictiveHorizontalPodAutoscalerReconciler struct {
	client.Client
	RESTMapper  meta.RESTMapper
	ScaleClient scale.ScalesGetter
	Scheme      *runtime.Scheme
	Gatherer    k8shorizmetrics.Gatherer
	Evaluator   k8shorizmetrics.Evaluator
	Predicter   prediction.Predicter
}

//+kubebuilder:rbac:groups=jamiethompson.me,resources=predictivehorizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=jamiethompson.me,resources=predictivehorizontalpodautoscalers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=jamiethompson.me,resources=predictivehorizontalpodautoscalers/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list
//+kubebuilder:rbac:groups=core,resources=replicationcontrollers/scale,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=deployments/scale;replicasets/scale;statefulsets/scale,verbs=get;update;patch
//+kubebuilder:rbac:groups=metrics.k8s.io,resources=*,verbs=get;list
//+kubebuilder:rbac:groups=custom.metrics.k8s.io,resources=*,verbs=get;list
//+kubebuilder:rbac:groups=external.metrics.k8s.io,resources=*,verbs=get;list

func (r *PredictiveHorizontalPodAutoscalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	instance := &jamiethompsonmev1alpha1.PredictiveHorizontalPodAutoscaler{}
	err := r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}

		logger.Error(err, "failed to get PredictiveHorizontalPodAutoscaler")
		return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
	}

	err = validation.Validate(instance)
	if err != nil {
		logger.Error(err, "invalid PredictiveHorizontalPodAutoscaler, disabling HPA+ until changed to be valid")
		// We stop processing here without requeueing since the HPA+ object is invalid, if changes are made to the spec that
		// make it valid it will be reconciled again and the validation checked
		return reconcile.Result{}, nil
	}

	scaleTargetRef := instance.Spec.ScaleTargetRef

	err = r.preScaleStatusCheck(ctx, instance)
	if err != nil {
		logger.Error(err, "failed pre scale status check", "scaleTargetRef", scaleTargetRef)
		return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
	}

	configMapName := fmt.Sprintf("hpa-plus-%s-data", instance.Name)
	hpaPlusData := &jamiethompsonmev1alpha1.PredictiveHorizontalPodAutoscalerData{}
	configMap := &corev1.ConfigMap{}

	// Check if configmap exists, if not create a blank one
	err = r.Client.Get(context.Background(),
		types.NamespacedName{
			Name:      configMapName,
			Namespace: instance.Namespace,
		},
		configMap)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			logger.V(1).Info("No configmap found for HPA+, creating a new one",
				"scaleTargetRef", scaleTargetRef)

			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("hpa-plus-%s-data", instance.Name),
					Namespace: instance.Namespace,
				},
			}

			configMap.SetOwnerReferences([]metav1.OwnerReference{{
				APIVersion: instance.APIVersion,
				Kind:       instance.Kind,
				Name:       instance.Name,
				UID:        instance.UID,
			}})

			hpaPlusData.ModelHistories = map[string]jamiethompsonmev1alpha1.ModelHistory{}

			data, err := json.Marshal(hpaPlusData)
			if err != nil {
				// Should not occur, panic
				panic(err)
			}

			configMap.Data = map[string]string{
				configMapDataKey: string(data),
			}

			err = r.Client.Create(ctx, configMap)
			if err != nil {
				logger.Error(err, "failed to create HPA+ configmap", "scaleTargetRef", scaleTargetRef)
				return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
			}

			// Ensure we continue processing even if no follow-up watch event is emitted for this ConfigMap.
			return reconcile.Result{RequeueAfter: defaultSyncPeriod}, nil
		}
		logger.Error(err, "failed to get HPA+ config map and data", "scaleTargetRef", scaleTargetRef)
		return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
	}

	err = json.Unmarshal([]byte(configMap.Data[configMapDataKey]), hpaPlusData)
	if err != nil {
		logger.Error(err, "failed to parse HPA+ data", "scaleTargetRef", scaleTargetRef)
		return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
	}

	syncPeriod := defaultSyncPeriod
	if instance.Spec.SyncPeriod != nil {
		syncPeriod = time.Duration(*instance.Spec.SyncPeriod) * time.Millisecond
	}

	now := time.Now().UTC()

	// Check the last scale of the HPA+, make sure we're not scaling too early
	lastScaleTime := instance.Status.LastScaleTime
	if lastScaleTime != nil && now.Add(-syncPeriod).Before(lastScaleTime.Time) {
		timeUntilReconcile := instance.Status.LastScaleTime.Time.Add(syncPeriod).Sub(now)
		logger.V(1).Info("Resource already scaled, queueing up reconcile for the next sync period",
			"scaleTargetRef", scaleTargetRef,
			"syncPeriod", syncPeriod,
			"timeUntilReconcile", timeUntilReconcile.Seconds())
		return reconcile.Result{RequeueAfter: timeUntilReconcile}, nil
	}

	// Get targeted scale subresource
	resourceGV, err := schema.ParseGroupVersion(scaleTargetRef.APIVersion)
	if err != nil {
		logger.Error(err, "failed to parse group version of target resource", "scaleTargetRef", scaleTargetRef)
		return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
	}

	targetGK := schema.GroupKind{Group: resourceGV.Group, Kind: scaleTargetRef.Kind}
	mapping, err := r.RESTMapper.RESTMapping(targetGK, resourceGV.Version)
	if err != nil {
		logger.Error(err, "failed to map target kind to resource", "scaleTargetRef", scaleTargetRef)
		return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
	}
	targetGR := mapping.Resource.GroupResource()

	scale, err := r.ScaleClient.Scales(instance.Namespace).Get(ctx, targetGR, scaleTargetRef.Name, metav1.GetOptions{})
	if err != nil {
		logger.Error(err, "failed to get scale subresource", "scaleTargetRef", scaleTargetRef)
		return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
	}

	currentReplicas := scale.Status.Replicas

	calculatedReplicas, metricValue, err := r.calculateReplicas(instance, scale)
	if err != nil {
		logger.Error(err, "failed to calculate replicas based on metrics",
			"scaleTargetRef", scaleTargetRef,
			"currentReplicas", currentReplicas)
		return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
	}

	// This function doesn't return any errors, since if it fails to process a model it will skip and continue
	// processing without that model's results
	predictedReplicas, modelPredictions, hpaPlusData := r.processModels(ctx, instance, hpaPlusData, now, currentReplicas,
		calculatedReplicas, metricValue)

	err = r.updateConfigMapData(ctx, configMap, hpaPlusData)
	if err != nil {
		logger.Error(err, "failed to update HPA+ configmap",
			"scaleTargetRef", scaleTargetRef)
		return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
	}

	decisionType := defaultDecisionType
	if instance.Spec.DecisionType != nil {
		decisionType = *instance.Spec.DecisionType
	}

	if modelPredictions == nil {
		modelPredictions = map[string]int32{}
	}

	includeHPABaseline := instance.Spec.IncludeHPA != nil && *instance.Spec.IncludeHPA
	modelInputsReady := len(predictedReplicas) > 0

	if !modelInputsReady {
		logger.Info("Model histories incomplete; falling back to HPA decision",
			"scaleTargetRef", scaleTargetRef,
			"calculatedReplicas", calculatedReplicas)
	}

	if includeHPABaseline {
		logger.Info("includeHPA enabled; including baseline HPA calculation in decision",
			"scaleTargetRef", scaleTargetRef,
			"calculatedReplicas", calculatedReplicas)
	}

	if includeHPABaseline || len(predictedReplicas) == 0 {
		predictedReplicas = append(predictedReplicas, calculatedReplicas)
		modelPredictions["hpa"] = calculatedReplicas
	}

	logger.Info("Preparing to select target replicas",
		"scaleTargetRef", scaleTargetRef,
		"decisionType", decisionType,
		"inputs", modelPredictions)

	targetReplicas := scalebehavior.DecideTargetReplicasByScalingStrategy(decisionType, predictedReplicas)
	logger.Info("Decision complete",
		"scaleTargetRef", scaleTargetRef,
		"decisionType", decisionType,
		"inputs", modelPredictions,
		"targetReplicas", targetReplicas)

	timestampedReplicaValue := jamiethompsonmev1alpha1.TimestampedReplicas{
		Time:     &metav1.Time{Time: now},
		Replicas: targetReplicas,
	}

	behavior := fillBehaviorDefaults(instance.Spec.Behavior)

	minReplicas := int32(defaultMinReplicas)
	if instance.Spec.MinReplicas != nil {
		minReplicas = *instance.Spec.MinReplicas
	}

	// Get the longest possible period that a scaling policy would look back for
	scaleUpLongestPolicyPeriod := scalebehavior.GetLongestPolicyPeriod(behavior.ScaleUp)
	scaleDownLongestPolicyPeriod := scalebehavior.GetLongestPolicyPeriod(behavior.ScaleDown)

	scaleUpEventHistory := scalebehavior.PruneTimestampedReplicasToWindow(
		instance.Status.ScaleUpEventHistory, scaleUpLongestPolicyPeriod, now)

	scaleDownEventHistory := scalebehavior.PruneTimestampedReplicasToWindow(
		instance.Status.ScaleDownEventHistory, scaleDownLongestPolicyPeriod, now)

	scaleUpReplicaHistory := scalebehavior.PruneTimestampedReplicasToWindow(
		instance.Status.ScaleUpReplicaHistory, *behavior.ScaleUp.StabilizationWindowSeconds, now)
	scaleUpReplicaHistory = append(scaleUpReplicaHistory, timestampedReplicaValue)

	scaleDownReplicaHistory := scalebehavior.PruneTimestampedReplicasToWindow(
		instance.Status.ScaleDownReplicaHistory, *behavior.ScaleDown.StabilizationWindowSeconds, now)
	scaleDownReplicaHistory = append(scaleDownReplicaHistory, timestampedReplicaValue)

	targetReplicas = scalebehavior.DecideTargetReplicasByBehavior(behavior, currentReplicas, targetReplicas, minReplicas,
		instance.Spec.MaxReplicas, scaleUpReplicaHistory, scaleDownReplicaHistory, scaleUpEventHistory,
		scaleDownEventHistory, now)

	// Only scale if the current replicas is different than the target
	if currentReplicas != targetReplicas {
		scale.Spec.Replicas = targetReplicas
		_, err := r.ScaleClient.Scales(instance.Namespace).Update(ctx, targetGR, scale, metav1.UpdateOptions{})
		if err != nil {
			logger.Error(err, "failed to update scale resource",
				"scaleTargetRef", scaleTargetRef,
				"currentReplicas", currentReplicas,
				"targetReplicas", targetReplicas)
			return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
		}

		scaleTime := time.Now().UTC()

		if targetReplicas > currentReplicas {
			// Scale up
			scaleEvent := jamiethompsonmev1alpha1.TimestampedReplicas{
				Time:     &metav1.Time{Time: scaleTime},
				Replicas: targetReplicas - currentReplicas,
			}
			instance.Status.ScaleUpEventHistory = append(instance.Status.ScaleUpEventHistory, scaleEvent)
			instance.Status.ScaleUpEventHistory = scalebehavior.PruneTimestampedReplicasToWindow(
				instance.Status.ScaleUpEventHistory,
				scaleUpLongestPolicyPeriod,
				scaleTime)
		} else {
			// Scale down
			scaleEvent := jamiethompsonmev1alpha1.TimestampedReplicas{
				Time:     &metav1.Time{Time: scaleTime},
				Replicas: currentReplicas - targetReplicas,
			}
			instance.Status.ScaleDownEventHistory = append(instance.Status.ScaleDownEventHistory, scaleEvent)
			instance.Status.ScaleDownEventHistory = scalebehavior.PruneTimestampedReplicasToWindow(
				instance.Status.ScaleDownEventHistory,
				scaleDownLongestPolicyPeriod,
				scaleTime)
		}

		instance.Status.LastScaleTime = &metav1.Time{Time: scaleTime}
	}

	instance.Status.DesiredReplicas = targetReplicas
	instance.Status.CurrentReplicas = currentReplicas
	instance.Status.ScaleDownReplicaHistory = scaleDownReplicaHistory
	instance.Status.ScaleUpReplicaHistory = scaleUpReplicaHistory
	err = r.Client.Status().Update(ctx, instance)
	if err != nil {
		logger.Error(err, "failed to update status of resource",
			"scaleTargetRef", scaleTargetRef,
			"currentReplicas", currentReplicas,
			"targetReplicas", targetReplicas,
			"scaleTime", now)
		return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
	}

	logger.V(0).Info("Scaled resource",
		"scaleTargetRef", scaleTargetRef,
		"currentReplicas", currentReplicas,
		"targetReplicas", targetReplicas)

	return reconcile.Result{RequeueAfter: syncPeriod}, nil

}

// updateConfigMapData updates the HPA+'s configmap and the data it holds
func (r *PredictiveHorizontalPodAutoscalerReconciler) updateConfigMapData(ctx context.Context, configMap *corev1.ConfigMap,
	hpaPlusData *jamiethompsonmev1alpha1.PredictiveHorizontalPodAutoscalerData) error {
	data, err := json.Marshal(hpaPlusData)
	if err != nil {
		// Should not occur, panic
		panic(err)
	}

	configMap.Data = map[string]string{
		configMapDataKey: string(data),
	}

	err = r.Client.Update(ctx, configMap)
	if err != nil {
		return fmt.Errorf("failed to update config map data: %w", err)
	}

	return nil
}

// processModels processes every model provided in the spec, it does not return any errors and will instead simply
// log if a model has failed to be processed, allowing the other models/the HPA calculated replicas to be used instead
func (r *PredictiveHorizontalPodAutoscalerReconciler) processModels(ctx context.Context,
	instance *jamiethompsonmev1alpha1.PredictiveHorizontalPodAutoscaler,
	hpaPlusData *jamiethompsonmev1alpha1.PredictiveHorizontalPodAutoscalerData, now time.Time, currentReplicas int32,
	calculatedReplicas int32, metricValue *float64) ([]int32, map[string]int32, *jamiethompsonmev1alpha1.PredictiveHorizontalPodAutoscalerData) {

	logger := log.FromContext(ctx)

	scaleTargetRef := instance.Spec.ScaleTargetRef

	modelNames := make([]string, len(instance.Spec.Models))
	for idx, model := range instance.Spec.Models {
		modelNames[idx] = model.Name
	}
	if len(modelNames) > 0 {
		logger.Info("Configured models for evaluation",
			"scaleTargetRef", scaleTargetRef,
			"models", modelNames)
	}

	// Set up a slice for predictions; the HPA result is added later depending on configuration/availability
	predictedReplicas := []int32{}
	modelPredictions := make(map[string]int32)

	// Add the calculated replicas to a list of past replicas
	for _, model := range instance.Spec.Models {
		logger.V(2).Info("Processing model to determine replica count",
			"scaleTargetRef", scaleTargetRef,
			"model", model.Name)

		perSyncPeriod := defaultPerSyncPeriod
		if model.PerSyncPeriod != nil {
			perSyncPeriod = *model.PerSyncPeriod
		}

		modelHistory, exists := hpaPlusData.ModelHistories[model.Name]
		if !exists || modelHistory.Type != model.Type {
			// Create new if model doesn't exist or has a type mismatch
			modelHistory = jamiethompsonmev1alpha1.ModelHistory{
				Type:              model.Type,
				SyncPeriodsPassed: 1,
				ReplicaHistory:    []jamiethompsonmev1alpha1.TimestampedReplicas{},
			}
		}

		if model.StartInterval != nil {
			if modelHistory.StartTime == nil {
				startTime := nextInterval(now, model.StartInterval.Duration)
				modelHistory.StartTime = &metav1.Time{Time: startTime}
				hpaPlusData.ModelHistories[model.Name] = modelHistory

				logger.V(1).Info("Skipping model for this sync period, start interval with no start time calculated, new start time calculated",
					"scaleTargetRef", scaleTargetRef,
					"startInterval", model.StartInterval.Duration,
					"startTime", modelHistory.StartTime,
					"timeUntilStart", modelHistory.StartTime.Sub(now),
					"model", model.Name)
				continue
			}

			if now.Before(modelHistory.StartTime.Time) {
				logger.V(1).Info("Skipping model for this sync period, before the start time",
					"scaleTargetRef", scaleTargetRef,
					"startInterval", model.StartInterval.Duration,
					"startTime", modelHistory.StartTime,
					"timeUntilStart", modelHistory.StartTime.Sub(now),
					"model", model.Name)
				continue
			}
		}

		// Calculate if it's been too long since the last data recorded
		if model.ResetDuration != nil && len(modelHistory.ReplicaHistory) > 0 {
			latest := modelHistory.ReplicaHistory[0].Time.Time
			for _, timestampedReplica := range modelHistory.ReplicaHistory {
				if timestampedReplica.Time.After(latest) {
					latest = timestampedReplica.Time.Time
				}
			}

			durationSinceLastData := now.Sub(latest)
			if durationSinceLastData > model.ResetDuration.Duration {
				// Clear replica history
				modelHistory.ReplicaHistory = []jamiethompsonmev1alpha1.TimestampedReplicas{}

				if model.StartInterval != nil {
					// Recalculate start time
					oldStartTime := modelHistory.StartTime
					startTime := nextInterval(now, model.StartInterval.Duration)

					modelHistory.StartTime = &metav1.Time{Time: startTime}
					hpaPlusData.ModelHistories[model.Name] = modelHistory

					logger.V(1).Info("Skipping model for this sync period, too much time has elapsed since the last data recorded, new start time calculated",
						"scaleTargetRef", scaleTargetRef,
						"startInterval", model.StartInterval.Duration,
						"latestData", metav1.Time{Time: latest},
						"durationSinceLastData", durationSinceLastData,
						"startIntervalResetDuration", model.ResetDuration.Duration,
						"oldStartTime", oldStartTime,
						"newStartTime", modelHistory.StartTime,
						"timeUntilStart", modelHistory.StartTime.Sub(now),
						"model", model.Name)
					continue
				}

				hpaPlusData.ModelHistories[model.Name] = modelHistory
				logger.V(1).Info("Clearing replica history, too much time has elapsed since the last data recorded",
					"scaleTargetRef", scaleTargetRef,
					"latestData", metav1.Time{Time: latest},
					"durationSinceLastData", durationSinceLastData,
					"startIntervalResetDuration", model.ResetDuration.Duration,
					"model", model.Name)
			}
		}

		shouldRunOnThisSyncPeriod := modelHistory.SyncPeriodsPassed >= perSyncPeriod

		modelHistory.ReplicaHistory = append(modelHistory.ReplicaHistory, jamiethompsonmev1alpha1.TimestampedReplicas{
			Time: &metav1.Time{
				Time: now,
			},
			Replicas: calculatedReplicas,
			Metric:   metricValue,
		})

		historyReady := modelHasSufficientHistory(&model, modelHistory.ReplicaHistory)

		if shouldRunOnThisSyncPeriod && historyReady {
			logger.V(1).Info("Using model to calculate predicted target replicas",
				"scaleTargetRef", scaleTargetRef,
				"model", model.Name)
			replicas, err := r.Predicter.GetPrediction(&model, modelHistory.ReplicaHistory)
			if err != nil {
				// Skip this model, errored out
				logger.Error(err, "failed to get predicted replica count",
					"scaleTargetRef", scaleTargetRef,
					"currentReplicas", currentReplicas,
					"targetReplicas", calculatedReplicas)
				continue
			}
			predictedReplicas = append(predictedReplicas, replicas)
			modelPredictions[model.Name] = replicas
			logger.Info("Model prediction ready",
				"scaleTargetRef", scaleTargetRef,
				"model", model.Name,
				"prediction", replicas)
			modelHistory.SyncPeriodsPassed = 1
		} else {
			if !historyReady {
				logger.V(1).Info("Skipping model for this sync period, not enough history recorded to satisfy history size",
					"scaleTargetRef", scaleTargetRef,
					"model", model.Name,
					"currentHistorySize", len(modelHistory.ReplicaHistory),
					"requiredHistorySize", requiredHistorySize(&model))
			} else {
				logger.V(1).Info("Skipping model for this sync period, should not run on this sync period",
					"scaleTargetRef", scaleTargetRef,
					"syncPeriodsPassed", modelHistory.SyncPeriodsPassed,
					"perSyncPeriod", perSyncPeriod,
					"model", model.Name)
			}
			modelHistory.SyncPeriodsPassed += 1
		}

		prunedHistory, err := r.Predicter.PruneHistory(&model, modelHistory.ReplicaHistory)
		if err != nil {
			// Skip this model, errored out
			logger.Error(err, "failed to prune replica history",
				"scaleTargetRef", scaleTargetRef)
			continue
		}

		modelHistory.ReplicaHistory = prunedHistory
		hpaPlusData.ModelHistories[model.Name] = modelHistory
	}

	// Delete any model data that exists without a corresponding model spec
	for modelName := range hpaPlusData.ModelHistories {
		exists := false
		for _, model := range instance.Spec.Models {
			if modelName == model.Name {
				exists = true
				break
			}
		}

		if !exists {
			delete(hpaPlusData.ModelHistories, modelName)
		}
	}

	return predictedReplicas, modelPredictions, hpaPlusData
}

// calculateReplicas does the HPA processing part of the autoscaling based on the metrics provided in the spec,
// returns the calculated value (the value the HPA would calculate based on these metrics) and the latest metric value if available.
func (r *PredictiveHorizontalPodAutoscalerReconciler) calculateReplicas(
	instance *jamiethompsonmev1alpha1.PredictiveHorizontalPodAutoscaler, scale *autoscalingv1.Scale) (int32, *float64, error) {
	cpuInitializationPeriod := defaultCPUInitializationPeriod
	if instance.Spec.CPUInitializationPeriod != nil {
		cpuInitializationPeriod = *instance.Spec.CPUInitializationPeriod
	}

	initialReadinessDelay := defaultInitialReadinessDelay
	if instance.Spec.InitialReadinessDelay != nil {
		initialReadinessDelay = *instance.Spec.InitialReadinessDelay
	}

	tolerance := defaultTolerance
	if instance.Spec.Tolerance != nil {
		tolerance = *instance.Spec.Tolerance
	}

	selector, err := labels.Parse(scale.Status.Selector)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to parse pod selector from scale subresource selector: %w", err)
	}

	// Gather K8s metrics using the spec
	metrics, err := r.Gatherer.GatherWithOptions(instance.Spec.Metrics, scale.Namespace, selector,
		time.Duration(cpuInitializationPeriod)*time.Second, time.Duration(initialReadinessDelay)*time.Second)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to gather metrics using provided metric specs: %w", err)
	}

	var metricValue *float64
	for _, metric := range metrics {
		if metric.Resource != nil && metric.Spec.Type == autoscalingv2.ResourceMetricSourceType &&
			metric.Spec.Resource != nil && metric.Spec.Resource.Name == corev1.ResourceCPU &&
			metric.Spec.Resource.Target.AverageUtilization != nil {
			usageRatio, currentUtilization, _, err := metricsclient.GetResourceUtilizationRatio(
				metric.Resource.PodMetricsInfo, metric.Resource.Requests, *metric.Spec.Resource.Target.AverageUtilization)
			if err == nil {
				val := float64(currentUtilization)
				metricValue = &val
			} else {
				_ = usageRatio
			}
			break
		}
	}

	// Calculate the targetReplicas using these metrics
	currentReplicas := scale.Status.Replicas
	calculatedReplicas, err := r.Evaluator.EvaluateWithOptions(metrics, currentReplicas, tolerance)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to evaluate metrics and calculate target replica count: %w", err)
	}

	return calculatedReplicas, metricValue, nil
}

// preScaleStatusCheck makes sure that the HPA+ status fields are correct before scaling, e.g. the reference field
// is set
func (r *PredictiveHorizontalPodAutoscalerReconciler) preScaleStatusCheck(ctx context.Context,
	instance *jamiethompsonmev1alpha1.PredictiveHorizontalPodAutoscaler) error {

	scaleTargetRef := instance.Spec.ScaleTargetRef

	reference := fmt.Sprintf("%s/%s", scaleTargetRef.Kind, scaleTargetRef.Name)
	if instance.Status.Reference != reference {
		instance.Status.Reference = reference
		err := r.Client.Status().Update(ctx, instance)
		if err != nil {
			return fmt.Errorf("failed to update status of resource: %w", err)
		}
	}

	return nil
}

func fillBehaviorDefaults(behavior *autoscalingv2.HorizontalPodAutoscalerBehavior) *autoscalingv2.HorizontalPodAutoscalerBehavior {
	// Defaults sourced from these sources:
	// https://github.com/kubernetes/enhancements/blob/7f681415a0011a0f6f98d9f112eeb7731f9eacd7/keps/sig-autoscaling/853-configurable-hpa-scale-velocity/README.md
	// https://github.com/kubernetes/kubernetes/blob/3e26e104bdf9d0dc3c4046d6350b93557c67f3f4/pkg/apis/autoscaling/v2/defaults.go

	if behavior == nil {
		return &autoscalingv2.HorizontalPodAutoscalerBehavior{
			ScaleDown: defaultDownscale(),
			ScaleUp:   defaultUpscale(),
		}
	}

	// We need to take a deep copy here, since we don't want any defaults we fill in to be persisted on the
	// actual object
	behavior = behavior.DeepCopy()

	behavior.ScaleUp = copyHPAScalingRules(behavior.ScaleUp, defaultUpscale())
	behavior.ScaleDown = copyHPAScalingRules(behavior.ScaleDown, defaultDownscale())

	return behavior
}

func copyHPAScalingRules(from, to *autoscalingv2.HPAScalingRules) *autoscalingv2.HPAScalingRules {
	if from == nil {
		return to
	}
	if from.SelectPolicy != nil {
		to.SelectPolicy = from.SelectPolicy
	}
	if from.StabilizationWindowSeconds != nil {
		to.StabilizationWindowSeconds = from.StabilizationWindowSeconds
	}
	if from.Policies != nil {
		to.Policies = from.Policies
	}
	return to
}

func defaultDownscale() *autoscalingv2.HPAScalingRules {
	return &autoscalingv2.HPAScalingRules{
		StabilizationWindowSeconds: int32Ptr(defaultDownscaleStabilization),
		SelectPolicy:               selectPolicyPtr(autoscalingv2.MaxChangePolicySelect),
		Policies: []autoscalingv2.HPAScalingPolicy{
			{
				Type:          autoscalingv2.PercentScalingPolicy,
				PeriodSeconds: defaultDownscalePercentagePolicyPeriodSeconds,
				Value:         defaultDownscalePercentagePolicyValue,
			},
		},
	}
}

func defaultUpscale() *autoscalingv2.HPAScalingRules {
	return &autoscalingv2.HPAScalingRules{
		StabilizationWindowSeconds: int32Ptr(0),
		SelectPolicy:               selectPolicyPtr(autoscalingv2.MaxChangePolicySelect),
		Policies: []autoscalingv2.HPAScalingPolicy{
			{
				Type:          autoscalingv2.PercentScalingPolicy,
				PeriodSeconds: defaultUpscalePercentagePolicyPeriodSeconds,
				Value:         defaultUpscalePercentagePolicyValue,
			},
			{
				Type:          autoscalingv2.PodsScalingPolicy,
				PeriodSeconds: defaultUpscalePodsPolicyPeriodSeconds,
				Value:         defaultUpscalePodsPolicyValue,
			},
		},
	}
}

func nextInterval(t time.Time, d time.Duration) time.Time {
	nextT := t.Round(d)
	if nextT.Before(t) {
		// If the calculated next time has already passed, lets add the duration onto it to get the next interval after
		// the time
		nextT = nextT.Add(d)
	}
	return nextT
}

func int32Ptr(i int32) *int32 {
	return &i
}

func selectPolicyPtr(policy autoscalingv2.ScalingPolicySelect) *autoscalingv2.ScalingPolicySelect {
	return &policy
}

func requiredHistorySize(model *jamiethompsonmev1alpha1.Model) int {
	switch model.Type {
	case jamiethompsonmev1alpha1.TypeLinear:
		if model.Linear != nil {
			return model.Linear.HistorySize
		}
	case jamiethompsonmev1alpha1.TypeXGBoost:
		if model.XGBoost != nil {
			return model.XGBoost.HistorySize
		}
	case jamiethompsonmev1alpha1.TypeArima:
		if model.Arima != nil && model.Arima.HistorySize != nil {
			return *model.Arima.HistorySize
		}
		return defaultArimaHistorySize
	case jamiethompsonmev1alpha1.TypeHoltWinters:
		if model.HoltWinters != nil {
			required := model.HoltWinters.SeasonalPeriods * model.HoltWinters.StoredSeasons
			statsmodelsMin := 2 * model.HoltWinters.SeasonalPeriods
			statsmodelsHeuristic := 10 + 2*(model.HoltWinters.SeasonalPeriods/2)
			if statsmodelsMin > required {
				required = statsmodelsMin
			}
			if statsmodelsHeuristic > required {
				required = statsmodelsHeuristic
			}
			return required
		}
	}
	return 0
}

func modelHasSufficientHistory(model *jamiethompsonmev1alpha1.Model, replicaHistory []jamiethompsonmev1alpha1.TimestampedReplicas) bool {
	required := requiredHistorySize(model)
	if required <= 0 {
		return true
	}
	return len(replicaHistory) >= required
}

// SetupWithManager sets up the controller with the Manager.
func (r *PredictiveHorizontalPodAutoscalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&jamiethompsonmev1alpha1.PredictiveHorizontalPodAutoscaler{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}
