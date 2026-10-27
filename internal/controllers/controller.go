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
	"errors"
	"fmt"
	"math"
	"time"

	"golang.org/x/time/rate"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"

	gathered "github.com/jthomperoo/k8shorizmetrics/v2/metrics"
	"github.com/jthomperoo/k8shorizmetrics/v2/metrics/podmetrics"
	"github.com/jthomperoo/k8shorizmetrics/v2/metricsclient"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/scale"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
	"github.com/cslab-ntua/HPA-Plus/internal/decision"
	"github.com/cslab-ntua/HPA-Plus/internal/forecastquality"
	"github.com/cslab-ntua/HPA-Plus/internal/history"
	"github.com/cslab-ntua/HPA-Plus/internal/hook"
	metricadapter "github.com/cslab-ntua/HPA-Plus/internal/metrics"
	"github.com/cslab-ntua/HPA-Plus/internal/observability"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	"github.com/cslab-ntua/HPA-Plus/internal/validation"
)

// HPA+ configuration constants
const (
	defaultSyncPeriod       = time.Duration(hpaplusv1alpha1.DefaultSyncPeriodMillis) * time.Millisecond
	defaultErrorRetryPeriod = 10 * time.Second
)

// HPA calculation configuration constants
const (
	defaultCPUInitializationPeriod = hpaplusv1alpha1.DefaultCPUInitializationSeconds
	defaultInitialReadinessDelay   = hpaplusv1alpha1.DefaultInitialReadinessSeconds
	defaultTolerance               = hpaplusv1alpha1.DefaultTolerance
	defaultPerSyncPeriod           = hpaplusv1alpha1.DefaultPerSyncPeriod
)

// HPA+ scale constraints
const (
	configMapDataKey        = "data"
	defaultArimaHistorySize = hpaplusv1alpha1.DefaultARIMAHistorySize
)

// HPAPlusReconciler reconciles a HPAPlus object
type HPAPlusReconciler struct {
	RetryPolicy          RetryPolicy
	WorkloadRunner       algorithm.Runner
	TrafficPrometheusURL string
	client.Client
	APIReader    client.Reader
	Now          func() time.Time
	RESTMapper   meta.RESTMapper
	ScaleClient  scale.ScalesGetter
	Scheme       *runtime.Scheme
	Gatherer     metricadapter.Gatherer
	Observations *metricadapter.Observations
	Evaluator    metricadapter.Evaluator
	Predicter    prediction.Predicter // Legacy injection; production uses Runtime.
	Runtime      prediction.Runtime
	HistoryStore history.Store
	Telemetry    *observability.Metrics
	Events       *observability.Events
}

type cpuMetricSnapshot = metricadapter.CPUObservation

//+kubebuilder:rbac:groups=hpa.plus,resources=hpapluses,verbs=get;list;watch
//+kubebuilder:rbac:groups=hpa.plus,resources=hpapluses/status,verbs=patch
//+kubebuilder:rbac:groups=hpa.plus,resources=hpapluses/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list
//+kubebuilder:rbac:groups=core,resources=replicationcontrollers,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=replicationcontrollers/scale,verbs=get;update
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;delete
//+kubebuilder:rbac:groups=apps,resources=deployments;replicasets;statefulsets,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=deployments/scale;replicasets/scale;statefulsets/scale,verbs=get;update
//+kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list
//+kubebuilder:rbac:groups=custom.metrics.k8s.io,resources=*,verbs=get;list
//+kubebuilder:rbac:groups=external.metrics.k8s.io,resources=*,verbs=get;list

func (r *HPAPlusReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reconcileErr error) {
	logger := log.FromContext(ctx).WithValues("namespace", req.Namespace, "resourceName", req.Name, "reconciliationID", string(uuid.NewUUID()))
	ctx = log.IntoContext(ctx, logger)
	health := reconcileHealth{phase: "ResourceRead"}
	// Last defer to run: controller-runtime must not log raw backend errors.
	defer func() {
		if reconcileErr != nil && !errors.Is(reconcileErr, context.Canceled) {
			log.FromContext(ctx).Error(observability.PublicError(reconcileErr), "Reconciliation failed", "phase", health.phase)
		}
		reconcileErr = observability.PublicError(reconcileErr)
	}()

	instance := &hpaplusv1alpha1.HPAPlus{}
	err := r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			r.Observations.Forget(req.Namespace + "/" + req.Name)
			r.syncModelSessions(req.Namespace, req.Name, nil, nil, "")
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}

		logger.Error(observability.PublicError(err), "failed to get HPAPlus")
		return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
	}

	if instance.DeletionTimestamp != nil {
		r.syncModelSessions(instance.Namespace, instance.Name, nil, nil, "")
		return reconcile.Result{}, nil
	}
	ctx = resourceLogContext(ctx, instance)
	logger = log.FromContext(ctx)
	health.phase = "ConfigurationValidation"
	defer func() {
		if ctx.Err() != nil {
			return
		}
		if err := r.publishHealth(ctx, instance, health, reconcileErr); err != nil {
			if ctx.Err() == nil {
				r.Events.StatusFailed(instance)
			}
			if reconcileErr != nil {
				reconcileErr = fmt.Errorf("%w; publishing health failed: %v", reconcileErr, err)
			} else {
				reconcileErr = err
			}
			result = reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}
		}
	}()
	err = validation.Validate(instance)
	if err != nil {
		logger.Error(observability.PublicError(err), "invalid HPAPlus, disabling HPA+ until changed to be valid")
		r.syncModelSessions(instance.Namespace, instance.Name, nil, nil, "")
		// We stop processing here without requeueing since the HPA+ object is invalid, if changes are made to the spec that
		// make it valid it will be reconciled again and the validation checked
		return reconcile.Result{}, nil
	}
	health.configurationValid = true
	health.phase = "StatusInitialization"

	err = r.preScaleStatusCheck(ctx, instance)
	if err != nil {
		logger.Error(observability.PublicError(err), "failed pre scale status check")
		return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
	}

	health.phase = "HistoryRead"
	historyConfigMap, hpaPlusData, err := r.loadHistory(ctx, instance)
	if err != nil {
		logger.Error(observability.PublicError(err), "failed to get HPA+ config map and data")
		return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
	}
	health.historyCapacity, err = planSnapshotCapacity(instance, historyConfigMap)
	if err != nil {
		// Planning is advisory. It must not strand scale-journal recovery or
		// change the safety checks around actual state and Scale writes.
		logger.Error(observability.PublicError(err), "failed to assess history capacity")
	}
	if hpaPlusData.PendingScale != nil {
		health.phase = "ScaleRecovery"
		ctx, _ = decisionContext(ctx, hpaPlusData.PendingScale.ID)
		if err := r.recoverScaleDecision(ctx, instance, hpaPlusData.PendingScale); err != nil {
			return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
		}
		// Re-read history and status before computing a new decision.
		return reconcile.Result{Requeue: true}, nil
	}

	syncPeriod := syncPeriodForInstance(instance)
	// Resolve identity before cooldown: replacing or retargeting a workload must
	// not reuse the previous workload's training data or scale-event history.
	health.phase = "TargetRead"
	scale, targetGR, err := r.getScaleTarget(ctx, instance)
	if err != nil {
		return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
	}
	health.phase = "HistoryBinding"
	hpaPlusData, err = r.ensureTargetHistory(ctx, instance, scale)
	if err != nil {
		return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
	}
	if err := r.syncTargetStatus(ctx, instance, scale, hpaPlusData.TargetIdentity); err != nil {
		return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
	}
	r.syncModelSessions(instance.Namespace, instance.Name, instance, hpaPlusData.ModelHistories, hpaPlusData.TargetIdentity)

	now := r.now()

	// Scale writes respect the existing sync-period cooldown. Request-demand
	// acquisition must continue independently, or a write at a forecast boundary
	// would suppress the following closed-window publication.
	var writeCooldownUntil time.Time
	lastScaleTime := instance.Status.LastScaleTime
	if instance.Spec.EffectiveMode() != hpaplusv1alpha1.ModeObserve && lastScaleTime != nil && now.Add(-syncPeriod).Before(lastScaleTime.Time) {
		health.phase = "Cooldown"
		timeUntilReconcile := instance.Status.LastScaleTime.Time.Add(syncPeriod).Sub(now)
		logger.V(1).Info("Resource already scaled, queueing up reconcile for the next sync period",
			"syncPeriod", syncPeriod,
			"timeUntilReconcile", timeUntilReconcile.Seconds())
		if !hasRequestDemand(instance) {
			return reconcile.Result{RequeueAfter: timeUntilReconcile}, nil
		}
		writeCooldownUntil = lastScaleTime.Add(syncPeriod)
	}

	currentReplicas := scale.Status.Replicas

	health.phase = "MetricEvaluation"
	metricsReady := false
	health.metricsAvailable = &metricsReady
	calculatedReplicas, cpuSnapshot, err := r.calculateReplicas(ctx, instance, scale)
	if err != nil {
		logger.Error(observability.PublicError(err), "failed to calculate replicas based on metrics",
			"currentReplicas", currentReplicas)
		return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
	}
	metricsReady = !cpuSnapshot.MetricsIncomplete
	health.metrics = cpuSnapshot.MetricStatus
	health.phase = "PredictionEvaluation"
	ctx, trace := decisionContext(ctx, "")
	logger = log.FromContext(ctx)

	// This function doesn't return any errors, since if it fails to process a model it will skip and continue
	// processing without that model's results
	predictedReplicas, modelPredictions, modelStatuses := r.processModels(ctx, instance, hpaPlusData, now, scale.Spec.Replicas,
		calculatedReplicas, cpuSnapshot, scale)
	health.models = modelStatuses
	for i, status := range modelStatuses {
		r.Telemetry.ObserveModelEvaluation(instance.Spec.Models[i].Type, status.State)
	}
	predictionsReady := len(predictedReplicas) > 0
	health.predictionsAvailable = &predictionsReady
	health.phase = "Decision"

	plan, err := decision.Plan(decision.Input{Spec: instance.Spec, Status: instance.Status, Now: now,
		Requested: scale.Spec.Replicas, Baseline: calculatedReplicas, Predictions: predictedReplicas,
		MetricsIncomplete: cpuSnapshot.MetricsIncomplete})
	if err != nil {
		return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
	}
	if deferredByCooldown(r.now(), writeCooldownUntil, plan.Desired, scale.Spec.Replicas, lastWriteRemovedPods(instance.Status)) {
		// Publish fresh measurements/model status through the deferred health
		// update, but do not journal or write a scaling decision yet.
		health.phase = "Cooldown"
		checkedAt := r.now()
		delay := timedDecisionRequeue(instance, modelStatuses, checkedAt, syncPeriod)
		return reconcile.Result{RequeueAfter: requestDemandRequeue(instance, checkedAt, delay, writeCooldownUntil)}, nil
	}
	if modelPredictions == nil {
		modelPredictions = map[string]int32{}
	}
	targetReplicas := plan.Desired
	scaleUpReplicaHistory, scaleDownReplicaHistory := plan.ScaleUpRecommendations, plan.ScaleDownRecommendations

	health.phase = "ScaleWrite"
	validity := decisionValidity{metricsUntil: cpuSnapshot.MetricsValidUntil, predictionsUntil: predictionInputsDeadline(modelStatuses)}
	if plan.RolloutMode == hpaplusv1alpha1.ModeObserve {
		health.phase = "ObservationPublication"
		err = r.observeScaleDecision(ctx, instance, scale, validity)
		if err == nil {
			trace.write = "observed"
		}
	} else {
		err = r.applyScaleDecision(ctx, instance, scale, targetGR, targetReplicas, scaleUpReplicaHistory, scaleDownReplicaHistory, validity)
	}
	candidates, omitted := decisionCandidates(modelPredictions)
	evidence := &hpaplusv1alpha1.DecisionEvidence{SelectedReplicas: plan.Selected, GuardedReplicas: plan.Guarded,
		BaselineIncluded: plan.BaselineIncluded, Constraints: append([]string{}, plan.Constraints...),
		Candidates: make([]hpaplusv1alpha1.DecisionCandidate, 0, len(candidates)), CandidatesOmitted: int32(omitted)}
	for _, candidate := range candidates {
		evidence.Candidates = append(evidence.Candidates, hpaplusv1alpha1.DecisionCandidate{Model: candidate.Model, Replicas: candidate.Replicas})
	}
	evidence.Models, evidence.ModelsOmitted = boundedModelStatuses(modelStatuses)
	boundDecisionEvidence(evidence)
	health.decision = &hpaplusv1alpha1.DecisionStatus{Evidence: evidence, ID: trace.id, Mode: plan.RolloutMode, CandidateMode: plan.Mode,
		ScaleWrite: trace.write, EvaluatedAt: metav1.Time{Time: now}, ObservedGeneration: instance.Generation,
		TargetUID: string(scale.UID), RequestedReplicas: scale.Spec.Replicas, ObservedReplicas: currentReplicas,
		BaselineReplicas: calculatedReplicas, BaselineDesiredReplicas: plan.BaselineDesired,
		ControlDesiredReplicas: plan.ControlDesired, DesiredReplicas: targetReplicas}
	outcome := "completed"
	if err != nil {
		outcome = "failed"
	}
	logger.Info("Scale decision", "outcome", outcome, "scaleWrite", trace.write, "errorReason", observability.ErrorReason(err),
		"rolloutMode", plan.RolloutMode, "baselineDesiredReplicas", plan.BaselineDesired, "controlDesiredReplicas", plan.ControlDesired,
		"mode", plan.Mode, "requestedReplicas", scale.Spec.Replicas, "observedReplicas", currentReplicas,
		"baselineReplicas", calculatedReplicas, "selectedReplicas", plan.Selected, "guardedReplicas", plan.Guarded,
		"targetReplicas", targetReplicas, "baselineIncluded", plan.BaselineIncluded, "constraints", plan.Constraints,
		"candidates", candidates, "candidatesOmitted", omitted)
	if err != nil {
		if errors.Is(err, errMetricsExpired) {
			metricsReady = false
			health.phase = "MetricEvaluation"
			health.metrics = nil
		}
		if errors.Is(err, errPredictionsExpired) {
			predictionsReady = expirePredictionReports(modelStatuses, r.now())
			health.phase = "PredictionEvaluation"
		}
		logger.Error(observability.PublicError(err), "failed to update status of resource",
			"currentReplicas", currentReplicas,
			"targetReplicas", targetReplicas,
			"scaleTime", now)
		return reconcile.Result{RequeueAfter: defaultErrorRetryPeriod}, err
	}

	health.phase = "Reconciled"
	r.Telemetry.ObserveRollout(plan.RolloutMode)
	if plan.RolloutMode != hpaplusv1alpha1.ModeObserve {
		r.Telemetry.ObserveDecision(plan.Mode, scale.Spec.Replicas, targetReplicas)
		for _, reason := range plan.Constraints {
			r.Telemetry.ObserveConstraint(reason)
		}
	}
	checkedAt := r.now()
	delay := timedDecisionRequeue(instance, modelStatuses, checkedAt, syncPeriod)
	return reconcile.Result{RequeueAfter: requestDemandRequeue(instance, checkedAt, delay, time.Time{})}, nil

}

// deferredByCooldown reports whether a decision waits for the write cooldown.
// A scale-up does not: delayed capacity costs requests, and the scale-up
// policies already bound how fast capacity grows. Nor does a scale-down that
// follows a scale-down: the cooldown keeps pods that were just added, and the
// scale-down policies already bound how fast capacity shrinks.
func deferredByCooldown(now, cooldownUntil time.Time, desired, requested int32, removedLast bool) bool {
	if !now.Before(cooldownUntil) || desired > requested {
		return false
	}
	return desired == requested || !removedLast
}

// lastWriteRemovedPods reports whether the newest recorded scale write removed pods.
func lastWriteRemovedPods(status hpaplusv1alpha1.HPAPlusStatus) bool {
	newest := func(events []hpaplusv1alpha1.TimestampedSample) *metav1.Time {
		var last *metav1.Time
		for _, event := range events {
			if event.Time != nil && (last == nil || event.Time.After(last.Time)) {
				last = event.Time
			}
		}
		return last
	}
	up, down := newest(status.ScaleUpEventHistory), newest(status.ScaleDownEventHistory)
	return down != nil && (up == nil || down.After(up.Time))
}

// processModels processes every model provided in the spec, it does not return any errors and will instead simply
// log if a model has failed to be processed, allowing the other models/the HPA calculated replicas to be used instead
func (r *HPAPlusReconciler) processModels(ctx context.Context,
	instance *hpaplusv1alpha1.HPAPlus,
	hpaPlusData *hpaplusv1alpha1.HPAPlusData, now time.Time,
	currentReplicas int32, calculatedReplicas int32, cpuSnapshot cpuMetricSnapshot, scales ...*autoscalingv1.Scale) ([]int32, map[string]int32, []hpaplusv1alpha1.ModelStatus) {

	logger := log.FromContext(ctx)

	// Set up a slice for predictions; the HPA result is added later depending on configuration/availability
	predictedReplicas := []int32{}
	modelPredictions := make(map[string]int32)
	predictionDeadlines := make(map[string]time.Time)
	historyEpochs := make(map[string]int64)
	statuses := make([]hpaplusv1alpha1.ModelStatus, len(instance.Spec.Models))
	fingerprints := make(map[string]string)

	// Add the calculated replicas to a list of past replicas
	for modelIndex, model := range instance.Spec.Models {
		ctx := log.IntoContext(ctx, logger.WithValues("modelName", observability.Field(model.Name), "modelType", observability.Field(model.Type)))
		logger := log.FromContext(ctx)
		status := &statuses[modelIndex]
		*status = hpaplusv1alpha1.ModelStatus{Name: model.Name, State: "WarmingUp", RequiredSamples: int64(requiredHistorySize(&model))}
		if instance.Status.ObservedGeneration == instance.Generation {
			for _, previous := range instance.Status.ModelStatuses {
				if previous.Name == model.Name {
					copy := previous.DeepCopy()
					status.LastAttemptTime, status.LastSuccessTime = copy.LastAttemptTime, copy.LastSuccessTime
					status.LastRunDurationMilliseconds, status.LastRunOutcome = copy.LastRunDurationMilliseconds, copy.LastRunOutcome
					break
				}
			}
		}
		logger.V(2).Info("Processing model to determine replica count",
			"model", observability.Field(model.Name))

		modelHistory, exists := hpaPlusData.ModelHistories[model.Name]
		if exists && modelHistory.Type == model.Type && model.HistoryMaxAge != nil {
			retained, err := r.retainModelHistory(ctx, instance, &model, hpaPlusData.TargetIdentity)
			if err != nil {
				status.State = "Unavailable"
				logger.Error(observability.PublicError(err), "failed to enforce model history retention", "model", observability.Field(model.Name))
				continue
			}
			modelHistory = retained
		}
		historyEpochs[model.Name] = modelHistory.HistoryEpoch
		if !exists || modelHistory.Type != model.Type {
			modelHistory = hpaplusv1alpha1.ModelHistory{
				Type:              model.Type,
				SyncPeriodsPassed: 0,
				ReplicaHistory:    []hpaplusv1alpha1.TimestampedSample{},
			}
		}
		status.HistorySamples = int64(countCPUHistory(modelHistory.ReplicaHistory))

		if model.StartInterval != nil && modelHistory.StartTime != nil && now.Before(modelHistory.StartTime.Time) {
			status.State = "Waiting"
			logger.V(1).Info("Skipping model for this sync period, before the start time",
				"startInterval", model.StartInterval.Duration,
				"startTime", modelHistory.StartTime,
				"timeUntilStart", modelHistory.StartTime.Sub(now),
				"model", observability.Field(model.Name))
			continue
		}

		if err := validateStoredHistory(modelHistory, r.now()); err != nil {
			status.State = "InvalidHistory"
			logger.Error(observability.PublicError(err), "skipping model with invalid history; future timestamps wait for clock recovery", "model", observability.Field(model.Name))
			continue
		}
		var oldest, newest *time.Time
		for _, sample := range modelHistory.ReplicaHistory {
			if sample.TotalCPUUsageMillicores == nil || sample.Time == nil {
				continue
			}
			if oldest == nil {
				oldest = &sample.Time.Time
			}
			newest = &sample.Time.Time
		}
		if oldest != nil {
			r.Telemetry.ObserveHistoryAge(model.Type, "oldest", r.now().Sub(*oldest))
			r.Telemetry.ObserveHistoryAge(model.Type, "newest", r.now().Sub(*newest))
		}

		historyReady := modelHasSufficientHistory(&model, modelHistory.ReplicaHistory)
		newSamplesSinceLastRun := countNewSamplesSinceLastRun(modelHistory)
		perSyncPeriod := defaultPerSyncPeriod
		if model.PerSyncPeriod != nil {
			perSyncPeriod = *model.PerSyncPeriod
		}
		shouldRunOnThisSyncPeriod := newSamplesSinceLastRun >= perSyncPeriod
		scopedModel := model
		scopedModel.SessionID = modelHistorySessionID(instance, model.Name, modelHistory.HistoryEpoch, cpuSnapshot.TargetIdentity)
		if cpuSnapshot.RequestPerPodMillicores != nil {
			scopedModel.CPURequestPerPodMillicores = *cpuSnapshot.RequestPerPodMillicores
		}
		if cpuSnapshot.TargetUtilizationPercentage != nil {
			scopedModel.TargetCPUUtilizationPercentage = *cpuSnapshot.TargetUtilizationPercentage
		}
		fingerprint, err := predictionFingerprint(instance, &scopedModel)
		if err != nil {
			status.State = "Failed"
			logger.Error(observability.PublicError(err), "failed to fingerprint model", "model", observability.Field(model.Name))
			continue
		}
		fingerprints[model.Name] = fingerprint
		if model.RequestDemand != nil {
			if len(scales) == 1 && scales[0] != nil {
				if replicas, deadline, usable := r.processRequestDemand(ctx, instance, &scopedModel, scales[0], cpuSnapshot, status, modelHistory.ReplicaHistory); usable {
					predictedReplicas = append(predictedReplicas, replicas)
					modelPredictions[model.Name] = replicas
					predictionDeadlines[model.Name] = deadline
					continue
				}
			} else {
				status.RequestDemand = &hpaplusv1alpha1.RequestDemandStatus{State: "unavailable", Source: "hold", FallbackReason: "resolved target unavailable"}
			}
		}
		if model.ForecastReliability != nil {
			status.Reliability = forecastquality.Status(modelHistory.Reliability, *model.ForecastReliability)
			if exists && hpaPlusData.ModelHistories[model.Name].Type == model.Type {
				updated, qualityErr := r.evaluateForecastReliability(ctx, instance, &scopedModel, modelHistory.HistoryEpoch, cpuSnapshot, fingerprint)
				if errors.Is(qualityErr, errReliabilityIdentityChanged) {
					status.State = "Unavailable"
					continue
				}
				if qualityErr != nil {
					// A diagnostic write failure must not suppress an otherwise valid
					// forecast. Existing completion/identity/expiry checks still apply.
					status.Reliability.State = "Unavailable"
					logger.Error(observability.PublicError(qualityErr), "failed to persist forecast reliability", "model", observability.Field(model.Name))
				} else {
					modelHistory = updated
					status.Reliability = forecastquality.Status(modelHistory.Reliability, *model.ForecastReliability)
				}
			}
			// Adapt only after a new real sample. Do not spin on the same window.
			if modelHistory.Reliability != nil && modelHistory.Reliability.AdaptationPending && newSamplesSinceLastRun > 0 {
				shouldRunOnThisSyncPeriod = true
			}
		}

		useCached := func() {
			if ctx.Err() != nil {
				return
			}
			availability, expires := assessCachedPrediction(instance, &scopedModel, modelHistory, fingerprint, r.now())
			cached := modelHistory.CachedPrediction
			if availability != cacheUsable {
				// Preserve an execution failure/backoff/warmup as the primary
				// reason, but explain a cadence skip or late fit with an expired cache.
				if availability == cacheExpired && (status.State == "Scheduled" || status.State == "Unavailable") {
					status.State = "Expired"
					status.ConsumedUntil = cached.ConsumedUntil.DeepCopy()
					status.ExpiresAt = &metav1.Time{Time: expires}
				}
				return
			}
			replicas := cached.Replicas
			if scopedModel.PredictivePolicy != nil {
				var boundary time.Time
				var err error
				replicas, boundary, err = prediction.TimedReplicas(cached.Trajectory, *scopedModel.PredictivePolicy, prediction.CPUObservation(&scopedModel).Capacity, currentReplicas, r.now())
				if err != nil {
					status.State = "Unavailable"
					return
				}
				if !boundary.IsZero() && boundary.Before(expires) {
					expires = boundary
				}
			}
			status.State = "Cached"
			status.ConsumedUntil = cached.ConsumedUntil.DeepCopy()
			status.ExpiresAt = &metav1.Time{Time: expires}
			predictedReplicas = append(predictedReplicas, replicas)
			modelPredictions[model.Name] = replicas
			predictionDeadlines[model.Name] = expires
			logger.V(1).Info("Using cached model prediction", "model", observability.Field(model.Name), "prediction", replicas, "sampleAge", r.now().Sub(cached.ConsumedUntil.Time).String(), "expiresAt", cached.ExpiresAt)
		}
		if model.PredictionMaxAge != nil && modelHistory.CachedPrediction != nil && modelHistory.CachedPrediction.Fingerprint != fingerprint {
			// Changed model/capacity needs a new fit even if its previous cadence was consumed.
			shouldRunOnThisSyncPeriod = true
		}

		if shouldRunOnThisSyncPeriod && historyReady {
			if r.RetryPolicy.inBackoff(modelHistory, fingerprint, r.now()) {
				status.State = "Backoff"
				logger.V(1).Info("Delaying failed model retry", "model", observability.Field(model.Name), "retryAfter", modelHistory.FailureState.RetryAfter)
				useCached()
				continue
			}
			logger.V(1).Info("Using model to calculate predicted target replicas",
				"model", observability.Field(model.Name))
			trainingDeadline := historyInputDeadline(&model, modelHistory.ReplicaHistory)
			if trainingDeadline != nil && !r.now().Before(*trainingDeadline) {
				status.State = "Expired"
				status.ExpiresAt = &metav1.Time{Time: *trainingDeadline}
				continue
			}
			completionFence := modelHistory.DeepCopy()
			status.LastAttemptTime = &metav1.Time{Time: r.now()}
			started := time.Now()
			observation := prediction.CPUObservation(&scopedModel)
			observation.Target, observation.SourceID, observation.At = cpuSnapshot.TargetIdentity, cpuSnapshot.SourceID, cpuSnapshot.SourceTime
			if cpuSnapshot.TotalUsageMillicores != nil {
				observation.Value = *cpuSnapshot.TotalUsageMillicores
			}
			predictionResult, err := r.modelRuntime().Predict(hook.WithResource(ctx, instance.Namespace, instance.Name), prediction.Request{Model: scopedModel, History: modelHistory.ReplicaHistory, Observation: &observation, Adapt: model.ForecastReliability != nil && modelHistory.Reliability != nil && modelHistory.Reliability.AdaptationPending})
			outcome := "success"
			if ctx.Err() != nil {
				outcome = "canceled"
			} else if errors.Is(err, prediction.ErrInsufficientHistory) {
				outcome = "insufficient_history"
			} else if errors.Is(err, context.DeadlineExceeded) {
				outcome = "timeout"
			} else if err != nil {
				outcome = "failed"
			}
			elapsed := time.Since(started)
			duration := elapsed.Milliseconds()
			status.LastRunDurationMilliseconds, status.LastRunOutcome = &duration, outcome
			if outcome == "success" {
				status.LastSuccessTime = &metav1.Time{Time: r.now()}
			}
			r.Telemetry.ObserveModelRun(model.Type, outcome, elapsed)
			if trainingDeadline != nil && !r.now().Before(*trainingDeadline) {
				status.State = "Expired"
				status.ExpiresAt = &metav1.Time{Time: *trainingDeadline}
				continue // Do not consume or back off an expired training window.
			}
			if err != nil {
				status.State = "Failed"
				if errors.Is(err, prediction.ErrInsufficientHistory) {
					status.State = "WarmingUp"
					logger.V(1).Info("Model is still warming up", "model", observability.Field(model.Name))
				} else {
					logger.Error(observability.PublicError(err), "failed to get predicted replica count",
						"currentReplicas", currentReplicas,
						"targetReplicas", calculatedReplicas)
					if ctx.Err() == nil {
						if persistErr := r.persistModelFailure(ctx, instance, model.Name, modelHistory.HistoryEpoch, hpaPlusData.TargetIdentity, fingerprint, trainingDeadline, completionFence); persistErr != nil {
							logger.Error(observability.PublicError(persistErr), "failed to persist model retry state", "model", observability.Field(model.Name))
						}
					}
				}
				if trainingDeadline != nil && !r.now().Before(*trainingDeadline) {
					status.State = "Expired"
					status.ExpiresAt = &metav1.Time{Time: *trainingDeadline}
					continue
				}
				useCached()
				continue
			}
			forecastDeadline, deadlineErr := freshForecastDeadline(instance, &scopedModel, predictionResult)
			if deadlineErr != nil {
				status.State = "Unavailable"
				continue
			}
			if forecastDeadline != nil {
				if trainingDeadline == nil || forecastDeadline.Before(*trainingDeadline) {
					trainingDeadline = forecastDeadline
				}
				status.ExpiresAt = &metav1.Time{Time: *trainingDeadline}
				if !r.now().Before(*trainingDeadline) {
					status.State = "Expired"
					continue
				}
			}
			if model.ForecastReliability != nil && modelHistory.Reliability != nil && modelHistory.Reliability.AdaptationPending && predictionResult.Adaptation == "" {
				predictionResult.Adaptation = "Unsupported"
			}
			cached := makeCachedPrediction(instance, &scopedModel, predictionResult, fingerprint, r.now(), modelHistory.ReplicaHistory)
			if cached != nil && scopedModel.PredictivePolicy != nil {
				cached.Trajectory = prediction.MergeCurrentInterval(predictionResult.Trajectory, modelHistory.CachedPrediction, fingerprint, r.now())
			}
			persisted, persistErr := r.persistModelResultWithReliability(ctx, instance, model.Name, modelHistory.HistoryEpoch, hpaPlusData.TargetIdentity, predictionResult.ConsumedUntil, cached, trainingDeadline, predictionResult.Verification, predictionResult.Adaptation, completionFence)
			if persistErr != nil {
				logger.Error(observability.PublicError(persistErr), "failed to persist model result", "model", observability.Field(model.Name))
			}
			if trainingDeadline != nil && !r.now().Before(*trainingDeadline) {
				status.State = "Expired"
				status.ExpiresAt = &metav1.Time{Time: *trainingDeadline}
				continue
			}
			if !persisted && persistErr == nil && predictionResult.ConsumedUntil != nil {
				status.State = "Unavailable"
				// History was reset/replaced while this forecast was running.
				continue
			}
			if trainingDeadline != nil {
				if !persisted {
					status.State = "Unavailable"
					continue
				}
				predictionDeadlines[model.Name] = *trainingDeadline
				status.ExpiresAt = &metav1.Time{Time: *trainingDeadline}
			}
			if model.PredictionMaxAge != nil {
				status.State = "Unavailable"
				if !persisted {
					continue
				}
				modelHistory.CachedPrediction = cached
				useCached()
				if status.State == "Cached" {
					status.State = "Ready"
				}
				continue
			}
			replicas := predictionResult.Replicas
			status.State = "Ready"
			if predictionResult.ConsumedUntil != nil {
				status.ConsumedUntil = &metav1.Time{Time: *predictionResult.ConsumedUntil}
			}
			predictedReplicas = append(predictedReplicas, replicas)
			modelPredictions[model.Name] = replicas
			logger.V(1).Info("Model prediction ready",
				"model", observability.Field(model.Name),
				"prediction", replicas)
		} else {
			if historyReady {
				status.State = "Scheduled"
			}
			useCached()
			if !historyReady {
				currentHistorySize := len(modelHistory.ReplicaHistory)
				if prediction.UsesCPUHistory(model.Type) {
					currentHistorySize = countCPUHistory(modelHistory.ReplicaHistory)
				}
				logger.V(1).Info("Skipping model for this sync period, not enough history recorded to satisfy history size",
					"model", observability.Field(model.Name),
					"currentHistorySize", currentHistorySize,
					"requiredHistorySize", requiredHistorySize(&model))
			} else {
				logger.V(1).Info("Skipping model for this sync period, should not run on this sync period",
					"newSamplesSinceLastRun", newSamplesSinceLastRun,
					"perSyncPeriod", perSyncPeriod,
					"model", observability.Field(model.Name))
			}
		}
	}

	// A later model may consume the remaining lifetime of an earlier candidate.
	// Recheck at the decision boundary, not only when each forecast completes.
	if len(statuses) > 0 {
		_, latest, err := r.loadHistory(ctx, instance)
		if err != nil {
			logger.Error(observability.PublicError(err), "failed to recheck prediction history")
			for i := range statuses {
				statuses[i].State = "Unavailable"
				statuses[i].ConsumedUntil = nil
				statuses[i].ExpiresAt = nil
			}
			finalizeRequestDemandSources(statuses, nil)
			return nil, map[string]int32{}, statuses
		}
		if latest.TargetIdentity != hpaPlusData.TargetIdentity {
			for i := range statuses {
				statuses[i].State = "Unavailable"
				statuses[i].ConsumedUntil = nil
				statuses[i].ExpiresAt = nil
			}
			finalizeRequestDemandSources(statuses, nil)
			return nil, map[string]int32{}, statuses
		}
		for i := range statuses {
			status := &statuses[i]
			history, ok := latest.ModelHistories[status.Name]
			if policy := instance.Spec.Models[i].ForecastReliability; policy != nil && ok && (status.Reliability == nil || status.Reliability.State != "Unavailable") {
				status.Reliability = forecastquality.Status(history.Reliability, *policy)
			}

			if ok && r.RetryPolicy.validFailureState(history.FailureState, fingerprints[status.Name], r.now()) {
				status.ConsecutiveFailures = history.FailureState.ConsecutiveFailures
				status.RetryAfter = history.FailureState.RetryAfter.DeepCopy()
			}
		}
		for name := range modelPredictions {
			if history, ok := latest.ModelHistories[name]; !ok || history.HistoryEpoch != historyEpochs[name] || validateStoredHistory(history, r.now()) != nil {
				delete(modelPredictions, name)
				delete(predictionDeadlines, name)
			}
		}
		for i := range statuses {
			if statuses[i].State != "Expired" {
				continue
			}
			history, ok := latest.ModelHistories[statuses[i].Name]
			if !ok || history.HistoryEpoch != historyEpochs[statuses[i].Name] || validateStoredHistory(history, r.now()) != nil {
				statuses[i].State = "Unavailable"
			}
		}
	}
	predictedReplicas = predictedReplicas[:0]
	for i, model := range instance.Spec.Models {
		if expires, ok := predictionDeadlines[model.Name]; ok && !r.now().Before(expires) {
			statuses[i].State = "Expired"
			delete(modelPredictions, model.Name)
		}
		if replicas, ok := modelPredictions[model.Name]; ok {
			statuses[i].ForecastReplicas = int32Ptr(replicas)
			predictedReplicas = append(predictedReplicas, replicas)
		} else {
			if statuses[i].State == "Ready" || statuses[i].State == "Cached" {
				statuses[i].State = "Unavailable"
			}
			if statuses[i].State != "Expired" {
				statuses[i].ConsumedUntil = nil
				statuses[i].ExpiresAt = nil
			}
		}
	}
	finalizeRequestDemandSources(statuses, modelPredictions)
	return predictedReplicas, modelPredictions, statuses
}

// calculateReplicas does the HPA processing part of the autoscaling based on the metrics provided in the spec,
// returns the calculated value (the value the HPA would calculate based on these metrics) and the latest metric value if available.
func (r *HPAPlusReconciler) calculateReplicas(
	ctx context.Context,
	instance *hpaplusv1alpha1.HPAPlus, scale *autoscalingv1.Scale) (resultReplicas int32, resultSnapshot cpuMetricSnapshot, resultErr error) {
	if r.Telemetry != nil {
		started := time.Now()
		defer func() {
			outcome := "complete"
			if ctx.Err() != nil {
				outcome = "canceled"
			} else if resultErr != nil {
				outcome = "unavailable"
			} else if resultSnapshot.MetricsIncomplete {
				outcome = "partial"
			}
			r.Telemetry.ObserveMetricEvaluation(outcome, time.Since(started))
		}()
	}
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
		return 0, cpuMetricSnapshot{}, fmt.Errorf("failed to parse pod selector from scale subresource selector: %w", err)
	}

	// Gather K8s metrics using the spec
	var metricMaxAge time.Duration
	if instance.Spec.MetricMaxAge != nil {
		metricMaxAge = time.Duration(*instance.Spec.MetricMaxAge) * time.Millisecond
		ctx = metricadapter.WithFreshness(ctx, metricMaxAge, r.now)
	}
	metrics, err := r.gatherObservations(ctx, instance, scale, selector,
		time.Duration(cpuInitializationPeriod)*time.Second, time.Duration(initialReadinessDelay)*time.Second, metricMaxAge)
	if err != nil {
		return 0, cpuMetricSnapshot{}, fmt.Errorf("failed to gather metrics using provided metric specs: %w", err)
	}
	metrics, validUntil := metricadapter.FreshObservations(metrics, r.now(), metricMaxAge)
	for _, metric := range metrics {
		if err := metricadapter.ValidateMetric(metric, scale.Status.Replicas); err != nil {
			return 0, cpuMetricSnapshot{}, fmt.Errorf("unsafe metric: %w", err)
		}
	}

	metricStatus := metricadapter.Summarize(metrics, scale.Status.Replicas)
	snapshot := cpuMetricSnapshot{TargetIdentity: scaleTargetIdentity(instance, scale), MetricStatus: &metricStatus, MetricsValidUntil: validUntil}
	for _, metric := range metrics {
		if metric.Resource != nil && metric.Spec.Type == autoscalingv2.ResourceMetricSourceType &&
			metric.Spec.Resource != nil && metric.Spec.Resource.Name == corev1.ResourceCPU &&
			metric.Spec.Resource.Target.AverageUtilization != nil {
			targetUtilization := *metric.Spec.Resource.Target.AverageUtilization
			snapshot.TargetUtilizationPercentage = &targetUtilization
			usageRatio, currentUtilization, _, err := metricsclient.GetResourceUtilizationRatio(
				metric.Resource.PodMetricsInfo, metric.Resource.Requests, *metric.Spec.Resource.Target.AverageUtilization)
			if err == nil {
				val := float64(currentUtilization)
				snapshot.Utilization = &val
			} else {
				_ = usageRatio
			}
			totalUsage, observedRequestPerPod := aggregateCPUMillicores(metric.Resource.PodMetricsInfo, metric.Resource.Requests)
			trainingMetric := metric
			// Partial pod populations are not total workload demand. Keep the
			// HPA missing-pod upscale logic, but never train or downscale on it.
			if totalUsage == nil || len(metric.Resource.MissingPods) > 0 || len(metric.Resource.IgnoredPods) > 0 ||
				metric.Resource.TotalPods != len(metric.Resource.PodMetricsInfo) {
				totalUsage = nil
				snapshot.MetricsIncomplete = true
				// Native HPA omits startup-window CPU by design. Prediction learns
				// measured consumption instead, but only if a separate read proves
				// every pod/container is present. Reactive inputs and guards stay intact.
				if historyGatherer, ok := r.Gatherer.(metricadapter.CPUHistoryGatherer); ok && hasCPUHistoryModel(instance) {
					raw, err := historyGatherer.GatherCPUHistory(ctx, metric.Spec, scale.Namespace, selector)
					if err == nil {
						fresh, until := metricadapter.FreshObservations([]*gathered.Metric{raw}, r.now(), metricMaxAge)
						if len(fresh) == 1 && completeCPUHistory(raw, scale.Status.Replicas) {
							totalUsage, observedRequestPerPod = aggregateCPUMillicores(raw.Resource.PodMetricsInfo, raw.Resource.Requests)
							trainingMetric = raw
							if until != nil && (snapshot.MetricsValidUntil == nil || until.Before(*snapshot.MetricsValidUntil)) {
								snapshot.MetricsValidUntil = until
							}
							_, utilization, _, usageErr := metricsclient.GetResourceUtilizationRatio(raw.Resource.PodMetricsInfo, raw.Resource.Requests, targetUtilization)
							if usageErr == nil {
								value := float64(utilization)
								snapshot.Utilization = &value
							}
						}
					}
				}
			}
			snapshot.TotalUsageMillicores = totalUsage
			snapshot.SourceID, snapshot.SourceTime = metricadapter.CPUSource(trainingMetric, r.now())
			requestPerPod, err := r.getTargetCPURequestPerPod(ctx, instance)
			capacitySource := "observed-minimum"
			snapshot.RequestPerPodMillicores = observedRequestPerPod
			if err == nil && requestPerPod != nil && observedRequestPerPod != nil && *requestPerPod < *observedRequestPerPod {
				snapshot.RequestPerPodMillicores = requestPerPod
				capacitySource = "workload-template-minimum"
			}
			if snapshot.RequestPerPodMillicores == nil {
				capacitySource = "unavailable"
			}
			log.FromContext(ctx).V(1).Info("Resolved predictive CPU capacity", "capacitySource", capacitySource,
				"requestPerPodMillicores", snapshot.RequestPerPodMillicores, "targetUtilizationPercentage", targetUtilization)
			break
		}
	}

	// Calculate the targetReplicas using these metrics
	currentReplicas := scale.Status.Replicas
	calculatedReplicas, incomplete, err := metricadapter.EvaluateAvailable(r.Evaluator, metrics, currentReplicas, tolerance)
	if err != nil {
		return 0, cpuMetricSnapshot{}, fmt.Errorf("failed to evaluate metrics and calculate target replica count: %w", err)
	}
	snapshot.MetricsIncomplete = snapshot.MetricsIncomplete || incomplete || len(metrics) != len(instance.Spec.Metrics)
	if snapshot.MetricsIncomplete && calculatedReplicas < scale.Spec.Replicas {
		calculatedReplicas = scale.Spec.Replicas
	}

	return calculatedReplicas, snapshot, nil
}

// Aggregate only complete, matched populations for training. The minimum of
// all listed requests bounds heterogeneous/VPA populations conservatively;
// an arithmetic mean can overstate capacity of replicas left after a downscale.
func aggregateCPUMillicores(metrics podmetrics.MetricsInfo, requests map[string]int64) (*int64, *int64) {
	if len(requests) == 0 {
		return nil, nil
	}
	minimum, requestSum := int64(math.MaxInt64), int64(0)
	for _, request := range requests {
		if request <= 0 || request > math.MaxInt64-requestSum {
			return nil, nil
		}
		requestSum += request
		if request < minimum {
			minimum = request
		}
	}
	total := int64(0)
	for name, metric := range metrics {
		if _, ok := requests[name]; !ok {
			return nil, nil
		}
		if metric.Value < 0 || metric.Value > math.MaxInt64-total {
			return nil, nil
		}
		total += metric.Value
	}
	if len(metrics) != len(requests) {
		return nil, &minimum
	}
	return &total, &minimum
}

func (r *HPAPlusReconciler) getTargetCPURequestPerPod(ctx context.Context, instance *hpaplusv1alpha1.HPAPlus) (*int64, error) {
	ref := instance.Spec.ScaleTargetRef
	if (ref.Kind == "ReplicationController" && ref.APIVersion != "v1") ||
		(ref.Kind != "ReplicationController" && ref.APIVersion != "apps/v1") {
		return nil, fmt.Errorf("unsupported scale target API version for CPU request lookup")
	}
	switch ref.Kind {
	case "Deployment", "ReplicaSet", "StatefulSet", "ReplicationController":
	default:
		return nil, fmt.Errorf("unsupported scale target kind for CPU request lookup: %s", ref.Kind)
	}
	// Read unstructured through the API reader: typed clients silently discard
	// newer native-sidecar and Pod-level resource fields in workload templates.
	target := &unstructured.Unstructured{}
	target.SetAPIVersion(ref.APIVersion)
	target.SetKind(ref.Kind)
	if err := r.reader().Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: instance.Namespace}, target); err != nil {
		return nil, err
	}
	spec, found, err := unstructured.NestedMap(target.Object, "spec", "template", "spec")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	value, err := metricadapter.PodResourceRequest(spec, corev1.ResourceCPU)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

// preScaleStatusCheck makes sure that the HPA+ status fields are correct before scaling, e.g. the reference field
// is set
func (r *HPAPlusReconciler) preScaleStatusCheck(ctx context.Context,
	instance *hpaplusv1alpha1.HPAPlus) error {
	scaleTargetRef := instance.Spec.ScaleTargetRef

	reference := fmt.Sprintf("%s/%s", scaleTargetRef.Kind, scaleTargetRef.Name)
	if instance.Status.Reference != reference {
		err := r.mutateStatus(ctx, instance, func(status *hpaplusv1alpha1.HPAPlusStatus) { status.Reference = reference })
		if err != nil {
			return fmt.Errorf("failed to update status of resource: %w", err)
		}
	}

	return nil
}

func fillBehaviorDefaults(behavior *autoscalingv2.HorizontalPodAutoscalerBehavior) *autoscalingv2.HorizontalPodAutoscalerBehavior {
	return decision.BehaviorDefaults(behavior)
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

func requiredHistorySize(model *hpaplusv1alpha1.Model) int {
	return prediction.RequiredHistorySize(model)
}

func modelHasSufficientHistory(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) bool {
	required := requiredHistorySize(model)
	if required <= 0 {
		return true
	}
	if prediction.UsesCPUHistory(model.Type) {
		return countCPUHistory(replicaHistory) >= required
	}
	return len(replicaHistory) >= required
}

func countCPUHistory(replicaHistory []hpaplusv1alpha1.TimestampedSample) int {
	return prediction.CountCPUHistory(replicaHistory)
}

func syncPeriodForInstance(instance *hpaplusv1alpha1.HPAPlus) time.Duration {
	syncPeriod := defaultSyncPeriod
	if instance.Spec.SyncPeriod != nil {
		syncPeriod = time.Duration(*instance.Spec.SyncPeriod) * time.Millisecond
	}
	return syncPeriod
}

func (r *HPAPlusReconciler) getScaleTarget(ctx context.Context,
	instance *hpaplusv1alpha1.HPAPlus,
) (*autoscalingv1.Scale, schema.GroupResource, error) {
	scaleTargetRef := instance.Spec.ScaleTargetRef
	resourceGV, err := schema.ParseGroupVersion(scaleTargetRef.APIVersion)
	if err != nil {
		return nil, schema.GroupResource{}, err
	}

	targetGK := schema.GroupKind{Group: resourceGV.Group, Kind: scaleTargetRef.Kind}
	mapping, err := r.RESTMapper.RESTMapping(targetGK, resourceGV.Version)
	if err != nil {
		return nil, schema.GroupResource{}, err
	}
	targetGR := mapping.Resource.GroupResource()

	scale, err := r.ScaleClient.Scales(instance.Namespace).Get(ctx, targetGR, scaleTargetRef.Name, metav1.GetOptions{})
	if err != nil {
		return nil, schema.GroupResource{}, err
	}

	return scale, targetGR, nil
}

func latestTimestamp(replicaHistory []hpaplusv1alpha1.TimestampedSample) *time.Time {
	var latest *time.Time
	for _, entry := range replicaHistory {
		if entry.Time == nil {
			continue
		}
		timestamp := entry.Time.Time
		if latest == nil || timestamp.After(*latest) {
			copy := timestamp
			latest = &copy
		}
	}
	return latest
}

func countNewSamplesSinceLastRun(modelHistory hpaplusv1alpha1.ModelHistory) int {
	if prediction.UsesCPUHistory(modelHistory.Type) {
		usable := make([]hpaplusv1alpha1.TimestampedSample, 0, len(modelHistory.ReplicaHistory))
		for _, sample := range modelHistory.ReplicaHistory {
			if sample.TotalCPUUsageMillicores != nil {
				usable = append(usable, sample)
			}
		}
		return countNewSamplesAfter(usable, modelHistory.LastModelRunTime)
	}
	return countNewSamplesAfter(modelHistory.ReplicaHistory, modelHistory.LastModelRunTime)
}

func countNewSamplesAfter(replicaHistory []hpaplusv1alpha1.TimestampedSample,
	lastProcessed *metav1.Time,
) int {
	latest := latestTimestamp(replicaHistory)
	if latest == nil {
		return 0
	}
	if lastProcessed == nil {
		return len(replicaHistory)
	}
	if latest.Before(lastProcessed.Time) {
		return len(replicaHistory)
	}

	count := 0
	for _, entry := range replicaHistory {
		if entry.Time == nil {
			continue
		}
		if entry.Time.Time.After(lastProcessed.Time) {
			count++
		}
	}
	return count
}

func (r *HPAPlusReconciler) persistModelResult(
	ctx context.Context,
	instance *hpaplusv1alpha1.HPAPlus,
	modelName string,
	historyEpoch int64,
	targetIdentity string,
	latestSample *time.Time,
	cached *hpaplusv1alpha1.CachedPrediction,
) (bool, error) {
	return r.persistModelResultChecked(ctx, instance, modelName, historyEpoch, targetIdentity, latestSample, cached, nil)
}

func (r *HPAPlusReconciler) persistModelResultChecked(ctx context.Context, instance *hpaplusv1alpha1.HPAPlus,
	modelName string, historyEpoch int64, targetIdentity string, latestSample *time.Time,
	cached *hpaplusv1alpha1.CachedPrediction, trainingDeadline *time.Time, expected ...*hpaplusv1alpha1.ModelHistory,
) (bool, error) {
	return r.persistModelResultWithReliability(ctx, instance, modelName, historyEpoch, targetIdentity, latestSample, cached, trainingDeadline, nil, "", expected...)
}

func (r *HPAPlusReconciler) persistModelResultWithReliability(ctx context.Context, instance *hpaplusv1alpha1.HPAPlus,
	modelName string, historyEpoch int64, targetIdentity string, latestSample *time.Time,
	cached *hpaplusv1alpha1.CachedPrediction, trainingDeadline *time.Time, verification *hpaplusv1alpha1.ForecastVerification, adaptation string, expected ...*hpaplusv1alpha1.ModelHistory,
) (bool, error) {
	if latestSample == nil {
		return false, nil
	}
	persisted := false
	err := r.mutateConfigMapData(ctx, instance, func(data *hpaplusv1alpha1.HPAPlusData) (bool, error) {
		persisted = false
		if trainingDeadline != nil && !r.now().Before(*trainingDeadline) {
			return false, errPredictionsExpired
		}
		if data.TargetIdentity != targetIdentity {
			return false, nil
		}
		modelHistory, exists := data.ModelHistories[modelName]
		if !exists || modelHistory.HistoryEpoch != historyEpoch || validateStoredHistory(modelHistory, r.now()) != nil {
			return false, nil
		}
		if !sameCompletionState(modelHistory, expected) {
			return false, nil
		}
		// An uncached success at an already consumed timestamp cannot publish a
		// distinct completion: it would leave the compared state unchanged.
		if cached == nil && modelHistory.FailureState == nil && modelHistory.LastModelRunTime != nil && modelHistory.LastModelRunTime.Time.Equal(*latestSample) {
			return false, nil
		}
		if modelHistory.LastModelRunTime != nil && modelHistory.LastModelRunTime.Time.After(*latestSample) {
			return false, nil
		}
		latest := latestTimestamp(modelHistory.ReplicaHistory)
		if latest == nil || latest.Before(*latestSample) {
			// The sampler reset/replaced the training history while this model was running.
			return false, nil
		}
		for _, m := range instance.Spec.Models {
			if m.Name == modelName && m.ForecastReliability == nil {
				modelHistory.Reliability = nil
			}
		}
		if modelHistory.Reliability != nil {
			if adaptation != "" {
				forecastquality.Adapted(modelHistory.Reliability, adaptation)
			}
			if verification != nil {
				copy := *verification
				copy.IssuedAt = r.now().UTC().Format(time.RFC3339Nano)
				forecastquality.Record(modelHistory.Reliability, &copy)
			} else {
				modelHistory.Reliability.Adaptation = "Unsupported"
				modelHistory.Reliability.AdaptationPending = false
			}
		}
		modelHistory.LastModelRunTime = &metav1.Time{Time: *latestSample}
		modelHistory.CachedPrediction = cached
		modelHistory.FailureState = nil
		data.ModelHistories[modelName] = modelHistory
		persisted = true
		return true, nil
	})
	return persisted && err == nil, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *HPAPlusReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(
			&hpaplusv1alpha1.HPAPlus{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		WithOptions(controller.Options{RateLimiter: scalingRateLimiter()}).
		Complete(r)
}

func scalingRateLimiter() workqueue.RateLimiter {
	// controller-runtime ignores RequeueAfter when Reconcile also returns an
	// error. Bound the per-resource error backoff so a temporary metrics outage
	// cannot strand scaling for the default limiter's sixteen-minute maximum.
	// Keep the default aggregate queue limit to avoid retry storms.
	return workqueue.NewMaxOfRateLimiter(
		workqueue.NewItemExponentialFailureRateLimiter(time.Second, defaultErrorRetryPeriod),
		&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)
}
