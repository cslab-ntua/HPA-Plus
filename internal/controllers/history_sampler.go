package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/cslab-ntua/HPA-Plus/internal/observability"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"time"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/history"
	metricadapter "github.com/cslab-ntua/HPA-Plus/internal/metrics"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	"github.com/cslab-ntua/HPA-Plus/internal/validation"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	historySamplerWorkers = 4
	historySampleTimeout  = 30 * time.Second
)

// HistorySampler records model training samples on a fixed wall-clock cadence, decoupled from reconcile duration.
type HistorySampler struct {
	Controller *HPAPlusReconciler
}

func NewHistorySampler(controller *HPAPlusReconciler) *HistorySampler {
	return &HistorySampler{Controller: controller}
}

// SetupWithManager registers an independent leader-owned controller. Informer
// initial lists and watch reconnects enqueue keys; status-only updates do not.
// The standard delayed work queue coalesces keys and serializes each resource.
func (s *HistorySampler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("history-sampler").
		For(&hpaplusv1alpha1.HPAPlus{}, builder.WithPredicates(samplingEvents())).
		WithOptions(controller.Options{MaxConcurrentReconciles: historySamplerWorkers,
			RateLimiter: workqueue.NewItemExponentialFailureRateLimiter(time.Second, historySampleTimeout)}).
		Complete(s)
}

func samplingEvents() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		return e.ObjectOld.GetUID() != e.ObjectNew.GetUID() ||
			e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() ||
			!e.ObjectOld.GetDeletionTimestamp().Equal(e.ObjectNew.GetDeletionTimestamp())
	}}
}

// nextSamplingDelay skips already-persisted whole-second slots, including for
// subsecond periods. Missed slots are not replayed after a slow attempt/restart.
func nextSamplingDelay(now time.Time, period time.Duration) time.Duration {
	next := now.Truncate(period).Add(period)
	earliest := samplingSlot(now, period).Add(time.Second)
	if next.Before(earliest) {
		next = earliest.Truncate(period)
		if next.Before(earliest) {
			next = next.Add(period)
		}
	}
	return next.Sub(now)
}

// A stable per-resource phase spreads periodic work without changing persisted
// slots, lifecycle deadlines or the four-worker concurrency bound.
func samplingJitter(instance *hpaplusv1alpha1.HPAPlus) time.Duration {
	if instance.UID == "" {
		return 0
	}
	limit := syncPeriodForInstance(instance) / 10
	if limit > time.Second {
		limit = time.Second
	}
	if limit <= 0 {
		return 0
	}
	sum := sha256.Sum256([]byte(instance.Namespace + "/" + instance.Name + "/" + string(instance.UID)))
	return time.Duration(binary.BigEndian.Uint64(sum[:8]) % uint64(limit))
}

// Optional independent CPU collection can retry an unavailable source in the
// first third of a slot. At least five seconds separate reads; successful slots
// remain consumed, including real zero usage. No missed historical slot is replayed.
func (s *HistorySampler) cpuRetryDeadline(instance *hpaplusv1alpha1.HPAPlus, slot, now time.Time) (time.Time, bool) {
	provider, ok := s.Controller.Gatherer.(metricadapter.IndependentCPUHistoryGatherer)
	period := syncPeriodForInstance(instance)
	deadline := slot.Add(period / 3)
	return deadline, ok && provider.IndependentCPUHistory() && period >= 15*time.Second && now.Before(deadline)
}

func (s *HistorySampler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("namespace", req.Namespace, "resourceName", req.Name, "reconciliationID", string(uuid.NewUUID())))
	result, err := s.sampleKeyResult(ctx, req.NamespacedName)
	return result, observability.PublicError(err)
}

func (s *HistorySampler) sampleKey(parent context.Context, key types.NamespacedName) error {
	_, err := s.sampleKeyResult(parent, key)
	return err
}

func (s *HistorySampler) sampleKeyResult(parent context.Context, key types.NamespacedName) (result ctrl.Result, sampleError error) {
	// Bound the complete attempt, including API reads, gathering and write retries.
	// A key carries no retained spec/UID; deletion/recreation and queued edits are
	// resolved from the authoritative reader when work starts.
	ctx, cancel := context.WithTimeout(parent, historySampleTimeout)
	defer cancel()
	defer func() {
		if sampleError != nil && !errors.Is(sampleError, context.Canceled) {
			log.FromContext(ctx).Error(observability.PublicError(sampleError), "Sampling failed")
		}
	}()
	if err := ctx.Err(); err != nil {
		return ctrl.Result{}, err
	}
	instance := &hpaplusv1alpha1.HPAPlus{}
	if err := s.Controller.reader().Get(ctx, key, instance); err != nil {
		if k8serrors.IsNotFound(err) {
			s.Controller.Observations.Forget(key.Namespace + "/" + key.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	ctx = resourceLogContext(ctx, instance)
	parent = resourceLogContext(parent, instance)
	if instance.DeletionTimestamp != nil {
		s.Controller.Observations.Forget(key.Namespace + "/" + key.Name)
		return ctrl.Result{}, nil
	}
	if len(instance.Spec.Models) == 0 {
		// Removing the final model must reclaim its history too; preserve the
		// journal and ownership metadata needed by baseline scaling/recovery.
		err := s.Controller.mutateConfigMapData(ctx, instance, func(data *hpaplusv1alpha1.HPAPlusData) (bool, error) {
			if len(data.ModelHistories) == 0 {
				return false, nil
			}
			data.ModelHistories = map[string]hpaplusv1alpha1.ModelHistory{}
			return true, nil
		})
		return ctrl.Result{}, s.reportSampling(parent, instance, samplingReport{outcome: "no_models", phase: "HistoryPreparation"}, err)
	}
	if err := validation.Validate(instance); err != nil {
		log.FromContext(ctx).V(1).Info("Skipping invalid HPAPlus while sampling",
			"name", instance.Name, "namespace", instance.Namespace, "errorReason", observability.ErrorReason(err))
		return ctrl.Result{}, s.reportSampling(parent, instance, samplingReport{outcome: "invalid"}, nil)
	}
	report := samplingReport{}
	sampleErr := s.sampleWithReport(ctx, instance, &report)
	if err := s.reportSampling(parent, instance, report, sampleErr); err != nil {
		return ctrl.Result{}, err
	}
	now := s.Controller.now()
	delay := nextSamplingDelay(now, syncPeriodForInstance(instance)) + samplingJitter(instance)
	if report.outcome == "missing_cpu" || report.outcome == "duplicate_source" {
		if deadline, retry := s.cpuRetryDeadline(instance, report.sampledSlot, now); retry {
			at := now.Add(5 * time.Second)
			if at.After(deadline) {
				at = deadline
			}
			if at.Sub(now) < delay {
				delay = at.Sub(now)
			}
		}
	}
	// If I/O crossed a slot boundary, sample the current slot promptly once.
	// Never enqueue each intervening historical slot as catch-up work.
	if samplingSlot(now, syncPeriodForInstance(instance)).After(report.sampledSlot) {
		delay = time.Millisecond
	}
	if report.nextCheck != nil && report.nextCheck.Sub(now) < delay {
		delay = report.nextCheck.Sub(now)
		if delay <= 0 {
			delay = time.Millisecond
		}
	}
	return ctrl.Result{RequeueAfter: delay}, nil
}

func (s *HistorySampler) sampleInstance(ctx context.Context, instance *hpaplusv1alpha1.HPAPlus) error {
	return s.sampleWithReport(ctx, instance, &samplingReport{})
}

func (s *HistorySampler) sampleWithReport(ctx context.Context,
	instance *hpaplusv1alpha1.HPAPlus, report *samplingReport,
) (sampleErr error) {
	report.outcome, report.phase = "not_due", "HistoryPreparation"
	if s.Controller.Telemetry != nil {
		started := time.Now()
		defer func() {
			outcome := report.outcome
			if ctx.Err() != nil {
				outcome = "canceled"
			} else if sampleErr != nil {
				outcome = "failed"
			}
			s.Controller.Telemetry.ObserveSampling(outcome, time.Since(started))
		}()
	}
	now := s.Controller.now()
	slotTime := samplingSlot(now, syncPeriodForInstance(instance))
	report.sampledSlot = slotTime

	var sampleDue bool
	err := s.Controller.mutateConfigMapData(ctx, instance, func(data *hpaplusv1alpha1.HPAPlusData) (bool, error) {
		changed, due, err := s.prepareSamplingState(instance, data, s.Controller.now(), slotTime)
		sampleDue = due
		report.nextCheck = nextHistoryCheck(instance, data, s.Controller.now())
		return changed, err
	})
	if err != nil {
		return fmt.Errorf("failed to prepare sampling state: %w", err)
	}
	if !sampleDue {
		return nil
	}
	report.outcome = "completed"

	report.phase = "TargetRead"
	scale, _, err := s.Controller.getScaleTarget(ctx, instance)
	if err != nil {
		return fmt.Errorf("failed to get scale target: %w", err)
	}
	report.phase = "HistoryBinding"
	if _, err := s.Controller.ensureTargetHistory(ctx, instance, scale); err != nil {
		return err
	}

	report.phase = "MetricEvaluation"
	var calculatedReplicas int32
	var cpuSnapshot cpuMetricSnapshot
	if provider, ok := s.Controller.Gatherer.(metricadapter.IndependentCPUHistoryGatherer); ok && provider.IndependentCPUHistory() && hasCPUHistoryModel(instance) {
		cpuSnapshot, err = s.Controller.collectIndependentCPU(ctx, instance, scale, provider)
		// CPU models train on CPU fields. The replica field records observed
		// capacity here instead of depending on a reactive calculation.
		calculatedReplicas = scale.Status.Replicas
	} else {
		calculatedReplicas, cpuSnapshot, err = s.Controller.calculateReplicas(ctx, instance, scale)
	}
	if err != nil {
		return fmt.Errorf("failed to calculate replicas for history sample: %w", err)
	}
	if cpuSnapshot.TotalUsageMillicores == nil {
		report.outcome = "missing_cpu"
	}

	report.phase = "HistoryWrite"
	return s.Controller.mutateHistoryChecked(ctx, instance, func(data *hpaplusv1alpha1.HPAPlusData) (bool, error) {
		if cpuSnapshot.MetricsValidUntil != nil && s.Controller.now().After(*cpuSnapshot.MetricsValidUntil) {
			return false, errMetricsExpired
		}
		if data.TargetIdentity != cpuSnapshot.TargetIdentity {
			return false, fmt.Errorf("scale target changed while sampling")
		}
		sourcesBefore := map[string]string{}
		for name, model := range data.ModelHistories {
			sourcesBefore[name] = model.LastObservationID
		}
		changed, err := s.applySample(instance, data, s.Controller.now(), slotTime, calculatedReplicas, cpuSnapshot)
		if err != nil {
			return false, err
		}
		// Partial populations can have a source ID without usable total CPU.
		// Preserve their missing-CPU outcome instead of calling them duplicates.
		if cpuSnapshot.TotalUsageMillicores != nil && cpuSnapshot.SourceID != "" {
			admitted := false
			for name, model := range data.ModelHistories {
				if model.LastObservationID != sourcesBefore[name] {
					admitted = true
				}
			}
			if !admitted {
				report.outcome = "duplicate_source"
			}
		}
		report.nextCheck = nextHistoryCheck(instance, data, s.Controller.now())
		for _, model := range instance.Spec.Models {
			if hasExpiredTraining(&model, data.ModelHistories[model.Name], s.Controller.now()) {
				return false, fmt.Errorf("history expired while preparing sample for model %s; retry", model.Name)
			}
		}
		if cpuSnapshot.MetricsValidUntil != nil && s.Controller.now().After(*cpuSnapshot.MetricsValidUntil) {
			return false, errMetricsExpired
		}
		return changed, err
	}, func(data *hpaplusv1alpha1.HPAPlusData) error {
		if cpuSnapshot.MetricsValidUntil != nil && s.Controller.now().After(*cpuSnapshot.MetricsValidUntil) {
			return errMetricsExpired
		}
		for _, model := range instance.Spec.Models {
			if hasExpiredTraining(&model, data.ModelHistories[model.Name], s.Controller.now()) {
				return fmt.Errorf("history expired before snapshot publication")
			}
		}
		return nil

	})
}

// Lifecycle deadlines can precede the next slot (for example a start gate in
// the middle of an hour-long period). Preserve timely preparation without scans.
func nextHistoryCheck(instance *hpaplusv1alpha1.HPAPlus, data *hpaplusv1alpha1.HPAPlusData, now time.Time) *time.Time {
	var next *time.Time
	consider := func(at time.Time) {
		if at.After(now) && (next == nil || at.Before(*next)) {
			value := at
			next = &value
		}
	}
	for _, model := range instance.Spec.Models {
		h := data.ModelHistories[model.Name]
		if model.StartInterval != nil && h.StartTime != nil {
			consider(h.StartTime.Time)
		}
		if model.ResetDuration != nil {
			if latest := latestTimestamp(h.ReplicaHistory); latest != nil {
				consider(latest.Add(model.ResetDuration.Duration).Add(time.Nanosecond))
			}
		}
		if model.HistoryMaxAge != nil {
			for _, row := range h.ReplicaHistory {
				if row.TotalCPUUsageMillicores != nil && row.Time != nil {
					consider(row.Time.Add(time.Duration(*model.HistoryMaxAge) * time.Millisecond))
				}
			}
		}
	}
	return next
}

// metav1.Time persists whole seconds. Use that same precision before comparing
// slots; otherwise a fractional slot is always newer than its persisted copy
// and can be recorded twice. Scheduling advances to the next distinct persisted slot.
func samplingSlot(now time.Time, period time.Duration) time.Time {
	return now.Truncate(period).UTC().Truncate(time.Second)
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
				ReplicaHistory:    []hpaplusv1alpha1.TimestampedSample{},
			}
			changed = true
		}
		if err := validateStoredHistory(modelHistory, now); err != nil {
			if errors.Is(err, history.ErrFutureWindow) {
				return false, false, err
			}
			if err := resetTrainingHistory(&modelHistory); err != nil {
				return false, false, err
			}
			changed = true
		}
		if prediction.UsesCPUHistory(model.Type) {
			// Legacy mixed histories could grow without bound during a partial CPU
			// outage because pruning counted only CPU-backed entries.
			usable := prediction.FilterCPUHistory(modelHistory.ReplicaHistory)
			if len(usable) != len(modelHistory.ReplicaHistory) {
				modelHistory.ReplicaHistory = usable
				changed = true
			}
			// Apply a reduced configured window even during a delayed start or
			// CPU outage. Otherwise a full store may never reach the append path
			// that normally prunes it. Keep each adapter's retention semantics.
			if required := requiredHistorySize(&model); required > 0 && len(modelHistory.ReplicaHistory) > required {
				pruned, err := s.Controller.modelRuntime().Prune(&model, modelHistory.ReplicaHistory)
				if err != nil {
					return false, false, fmt.Errorf("failed to prune model history for %s: %w", model.Name, err)
				}
				if len(pruned) < len(modelHistory.ReplicaHistory) {
					modelHistory.ReplicaHistory = pruned
					changed = true
				}
			}
		}

		priorLatest := latestTimestamp(modelHistory.ReplicaHistory)
		aged, err := pruneAgedHistory(&model, &modelHistory, now)
		if err != nil {
			return false, false, err
		}
		changed = changed || aged

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

		if model.ResetDuration != nil && priorLatest != nil {
			latest := priorLatest
			if latest != nil && now.Sub(*latest) > model.ResetDuration.Duration {
				if err := resetTrainingHistory(&modelHistory); err != nil {
					return false, false, err
				}
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
		if modelHistory.LastSampleTime != nil && (latest == nil || modelHistory.LastSampleTime.After(*latest)) {
			latest = &modelHistory.LastSampleTime.Time
		}
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
				ReplicaHistory:    []hpaplusv1alpha1.TimestampedSample{},
			}
			changed = true
		}
		// The ConfigMap is re-read after gathering; validate again in case it
		// changed between preparation and this write.
		if err := validateStoredHistory(modelHistory, now); err != nil {
			if errors.Is(err, history.ErrFutureWindow) {
				return false, err
			}
			if err := resetTrainingHistory(&modelHistory); err != nil {
				return false, err
			}
			changed = true
		}

		priorLatest := latestTimestamp(modelHistory.ReplicaHistory)
		aged, err := pruneAgedHistory(&model, &modelHistory, now)
		if err != nil {
			return false, err
		}
		changed = changed || aged

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

		if model.ResetDuration != nil && priorLatest != nil {
			latest := priorLatest
			if latest != nil && now.Sub(*latest) > model.ResetDuration.Duration {
				if err := resetTrainingHistory(&modelHistory); err != nil {
					return false, err
				}
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
		if modelHistory.LastSampleTime != nil && (latest == nil || modelHistory.LastSampleTime.After(*latest)) {
			latest = &modelHistory.LastSampleTime.Time
		}
		if latest != nil && !slotTime.After(*latest) {
			hpaPlusData.ModelHistories[model.Name] = modelHistory
			continue
		}
		observationTime := slotTime
		_, retryCPU := s.cpuRetryDeadline(instance, slotTime, now)
		if prediction.UsesCPUHistory(model.Type) && (cpuSnapshot.TotalUsageMillicores == nil || (modelHistory.LastObservationID != "" && cpuSnapshot.SourceID == "")) {
			if !retryCPU {
				modelHistory.LastSampleTime = &metav1.Time{Time: slotTime}
				changed = true
			}
			hpaPlusData.ModelHistories[model.Name] = modelHistory
			continue
		}

		// Persist source deduplication with the same CAS as training history.
		// Repeated or older observations never enter history. An independent
		// source may retry within the bounded early window; otherwise consume
		// the slot without a sample.
		if cpuSnapshot.SourceID != "" {
			previous, _ := time.Parse(time.RFC3339Nano, modelHistory.LastObservationTime)
			if cpuSnapshot.SourceID == modelHistory.LastObservationID || (!previous.IsZero() && !cpuSnapshot.SourceTime.After(previous)) {
				if !retryCPU {
					modelHistory.LastSampleTime = &metav1.Time{Time: slotTime}
					changed = true
				}
				hpaPlusData.ModelHistories[model.Name] = modelHistory
				continue
			}
			// Forecast time starts at the measurement, not the later collection
			// slot. Keep the existing whole-second wire representation, and do
			// not append a source behind a legacy slot-stamped history entry.
			observationTime = cpuSnapshot.SourceTime.UTC().Truncate(time.Second)
			if observationTime.IsZero() || observationTime.After(now) || (priorLatest != nil && !observationTime.After(*priorLatest)) {
				if !retryCPU {
					modelHistory.LastSampleTime = &metav1.Time{Time: slotTime}
					changed = true
				}
				hpaPlusData.ModelHistories[model.Name] = modelHistory
				continue
			}
			modelHistory.LastObservationID = cpuSnapshot.SourceID
			modelHistory.LastObservationTime = cpuSnapshot.SourceTime.UTC().Format(time.RFC3339Nano)
		}
		modelHistory.LastSampleTime = &metav1.Time{Time: slotTime}
		if model.HistoryMaxAge != nil && !now.Before(slotTime.Add(time.Duration(*model.HistoryMaxAge)*time.Millisecond)) {
			return false, fmt.Errorf("sampling slot exceeded historyMaxAge for model %s", model.Name)
		}
		modelHistory.ReplicaHistory = append(modelHistory.ReplicaHistory, hpaplusv1alpha1.TimestampedSample{
			Time:                           &metav1.Time{Time: observationTime},
			Replicas:                       calculatedReplicas,
			Metric:                         cpuSnapshot.Utilization,
			TotalCPUUsageMillicores:        cpuSnapshot.TotalUsageMillicores,
			RequestPerPodMillicores:        cpuSnapshot.RequestPerPodMillicores,
			TargetCPUUtilizationPercentage: cpuSnapshot.TargetUtilizationPercentage,
		})

		prunedHistory, err := s.Controller.modelRuntime().Prune(&model, modelHistory.ReplicaHistory)
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
