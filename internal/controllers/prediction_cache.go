package controllers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func predictionFingerprint(instance *api.HPAPlus, model *api.Model) (string, error) {
	// Runtime capacity fields are deliberately excluded from Model's JSON, so
	// include them explicitly. Generation also invalidates changes to metrics/target.
	payload, err := json.Marshal(struct {
		UID            string
		Generation     int64
		Spec           api.HPAPlusSpec
		Model          *api.Model
		CPURequest     int64
		CPUUtilization int32
		SessionID      string
	}{string(instance.UID), instance.Generation, instance.Spec, model, model.CPURequestPerPodMillicores, model.TargetCPUUtilizationPercentage, model.SessionID})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:]), nil
}

func predictionHorizon(instance *api.HPAPlus, model *api.Model) time.Duration {
	var horizon time.Duration
	switch model.Type {
	case api.TypeLinear:
		if model.Linear != nil {
			horizon = time.Duration(model.Linear.LookAhead) * time.Millisecond
		}
	case api.TypeArima:
		if model.Arima != nil {
			horizon = time.Duration(model.Arima.LookAhead) * time.Millisecond
		}
	case api.TypeXGBoost:
		if model.XGBoost != nil {
			horizon = time.Duration(model.XGBoost.LookAhead) * time.Millisecond
		}
	case api.TypeLightGBM:
		if model.LightGBM != nil {
			horizon = time.Duration(model.LightGBM.LookAhead) * time.Millisecond
		}
	case api.TypeHoltWinters:
		horizon = syncPeriodForInstance(instance)
		if model.HoltWinters != nil && model.HoltWinters.LookAhead != nil {
			horizon = time.Duration(*model.HoltWinters.LookAhead) * time.Millisecond
		}
	}
	return horizon
}

func predictionLifetime(instance *api.HPAPlus, model *api.Model) time.Duration {
	if model.PredictionMaxAge == nil {
		return 0
	}
	age := time.Duration(*model.PredictionMaxAge) * time.Millisecond
	horizon := predictionHorizon(instance, model)
	if model.PredictivePolicy != nil {
		return age
	}
	if horizon < age {
		return horizon
	}
	return age
}

// freshForecastDeadline bounds the actual forecast by its data-relative target.
// Legacy injected runtimes without provenance retain their existing test seam.
// PredictionMaxAge still controls reuse; no uncertainty weighting is inferred.
func freshForecastDeadline(instance *api.HPAPlus, model *api.Model, result prediction.Result) (*time.Time, error) {
	var intervalEnd *time.Time
	if usesExtendedTimedInterval(instance, model) {
		if result.ConsumedUntil == nil {
			return nil, fmt.Errorf("timed forecast lacks consumed timestamp")
		}
		end, err := prediction.ModelTrajectoryExpiry(model, result.Trajectory, *result.ConsumedUntil)
		if err != nil {
			return nil, err
		}
		age := result.ConsumedUntil.Add(time.Duration(*model.PredictionMaxAge) * time.Millisecond)
		if age.Before(end) {
			end = age
		}
		intervalEnd = &end
	}
	if result.Diagnostics == nil {
		return intervalEnd, nil
	}

	d := result.Diagnostics
	if err := d.Validate(); err != nil {
		return nil, err
	}
	if d.Model != model.Type || result.ConsumedUntil == nil {
		return nil, fmt.Errorf("forecast identity mismatch")
	}
	end, err := time.Parse(time.RFC3339Nano, d.HistoryEnd)
	if err != nil || !end.Equal(*result.ConsumedUntil) {
		return nil, fmt.Errorf("forecast consumed timestamp mismatch")
	}
	target, err := time.Parse(time.RFC3339Nano, d.ForecastAt)
	if err != nil {
		return nil, err
	}
	// Discrete rounding may move the engine's virtual target forward. The requested
	// horizon remains a conservative freshness ceiling, except implicit Holt's step.
	horizon := predictionHorizon(instance, model)
	implicitHolt := model.Type == api.TypeHoltWinters && model.HoltWinters != nil && model.HoltWinters.LookAhead == nil
	if intervalEnd != nil {
		last, _ := time.Parse(time.RFC3339Nano, result.Trajectory[len(result.Trajectory)-1].At)
		if !last.Equal(target) {
			return nil, fmt.Errorf("trajectory provenance target mismatch")
		}
		target = *intervalEnd
	} else if horizon > 0 && !implicitHolt {
		cap := result.ConsumedUntil.Add(horizon)
		if cap.Before(target) {
			target = cap
		}
	}
	if model.PredictionMaxAge != nil {
		age := result.ConsumedUntil.Add(time.Duration(*model.PredictionMaxAge) * time.Millisecond)
		if age.Before(target) {
			target = age
		}
	}
	return &target, nil
}

type cacheAvailability int

const (
	cacheUnavailable cacheAvailability = iota
	cacheExpired
	cacheUsable
)

// assessCachedPrediction separates expiry from invalidation and returns the
// effective deadline. Persisted metadata may shorten, but never extend, policy.
func assessCachedPrediction(instance *api.HPAPlus, model *api.Model, history api.ModelHistory, fingerprint string, now time.Time) (cacheAvailability, time.Time) {
	cached := history.CachedPrediction
	if model.PredictionMaxAge == nil || cached == nil || cached.Replicas < 0 || cached.Fingerprint != fingerprint || fingerprint == "" {
		return cacheUnavailable, time.Time{}
	}
	if !modelHasSufficientHistory(model, history.ReplicaHistory) || cached.CompletedAt.IsZero() || cached.ConsumedUntil.IsZero() ||
		cached.ExpiresAt.IsZero() || cached.CompletedAt.After(now) || cached.ConsumedUntil.After(cached.CompletedAt.Time) ||
		!cached.ExpiresAt.After(cached.ConsumedUntil.Time) {
		return cacheUnavailable, time.Time{}
	}
	// A reset or replacement history cannot revive an older prediction. Ordinary
	// rolling-window pruning may remove the consumed sample while keeping newer ones.
	latest := latestTimestamp(history.ReplicaHistory)
	if latest != nil && model.ResetDuration != nil && now.Sub(*latest) > model.ResetDuration.Duration {
		return cacheUnavailable, time.Time{}
	}
	if latest == nil || latest.Before(cached.ConsumedUntil.Time) {
		return cacheUnavailable, time.Time{}
	}
	lifetime := predictionLifetime(instance, model)
	if lifetime <= 0 {
		return cacheUnavailable, time.Time{}
	}
	expires := cached.ConsumedUntil.Add(lifetime)
	if usesExtendedTimedInterval(instance, model) {
		end, err := cachedTrajectoryDeadline(model, cached)
		if err != nil {
			return cacheUnavailable, time.Time{}
		}
		if end.Before(expires) {
			expires = end
		}
	}

	if cached.ExpiresAt.Before(&metav1.Time{Time: expires}) {
		expires = cached.ExpiresAt.Time
	}
	if model.HistoryMaxAge != nil {
		if cached.HistoryStart == nil || cached.HistoryStart.IsZero() || cached.HistoryStart.After(cached.ConsumedUntil.Time) {
			return cacheUnavailable, time.Time{}
		}
		trainingExpiry := cached.HistoryStart.Add(time.Duration(*model.HistoryMaxAge) * time.Millisecond)
		if trainingExpiry.Before(expires) {
			expires = trainingExpiry
		}
	}
	if !now.Before(expires) {
		return cacheExpired, expires
	}
	if hasExpiredTraining(model, history, now) {
		return cacheUnavailable, time.Time{}
	}
	return cacheUsable, expires
}

// A merged cache has a fresh uniform grid and optionally one preserved current
// interval. That interval can begin AFTER the newest source timestamp: a delayed
// observation is still older than the interval covering forecast completion.
// Identify the one allowed prefix structurally, rather than discarding points
// solely by their relation to ConsumedUntil. Neither grid nor cache expiry changes.
func cachedTrajectoryDeadline(model *api.Model, cached *api.CachedPrediction) (time.Time, error) {
	points := cached.Trajectory
	if err := algorithm.ValidateForecastTrajectory(points); err != nil {
		return time.Time{}, err
	}
	end, err := prediction.ModelTrajectoryExpiry(model, points, cached.ConsumedUntil.Time)
	if err == nil || len(points) < 2 {
		return end, err
	}
	at, _ := time.Parse(time.RFC3339Nano, points[0].At)
	until, _ := time.Parse(time.RFC3339Nano, points[0].Until)
	firstFresh, _ := time.Parse(time.RFC3339Nano, points[1].At)
	// CompletedAt persists at whole-second precision. Its actual instant lies
	// within that second, so use an exclusive one-second upper bound.
	if !at.Before(cached.CompletedAt.Add(time.Second)) || !until.After(cached.CompletedAt.Time) || until.After(firstFresh) {
		return time.Time{}, fmt.Errorf("invalid preserved current interval")
	}
	return prediction.ModelTrajectoryExpiry(model, points[1:], cached.ConsumedUntil.Time)
}

func makeCachedPrediction(instance *api.HPAPlus, model *api.Model, result prediction.Result, fingerprint string, completed time.Time, history []api.TimestampedSample) *api.CachedPrediction {
	if model.PredictionMaxAge == nil || result.ConsumedUntil == nil {
		return nil
	}
	cached := &api.CachedPrediction{Trajectory: append([]api.CPUForecastPoint(nil), result.Trajectory...), Replicas: result.Replicas, Fingerprint: fingerprint,
		ConsumedUntil: metav1.Time{Time: *result.ConsumedUntil}, CompletedAt: metav1.Time{Time: completed},
		ExpiresAt: metav1.Time{Time: result.ConsumedUntil.Add(predictionLifetime(instance, model))}}
	if forecastDeadline, err := freshForecastDeadline(instance, model, result); err == nil && forecastDeadline != nil && forecastDeadline.Before(cached.ExpiresAt.Time) {
		cached.ExpiresAt = metav1.Time{Time: *forecastDeadline}
	}
	if deadline := historyInputDeadline(model, history); deadline != nil {
		cached.HistoryStart = &metav1.Time{Time: deadline.Add(-time.Duration(*model.HistoryMaxAge) * time.Millisecond)}
		if deadline.Before(cached.ExpiresAt.Time) {
			cached.ExpiresAt = metav1.Time{Time: *deadline}
		}
	}
	return cached
}

// predictionInputsDeadline guards the complete candidate set used by any
// decision strategy (including mean/median). If any opted-in candidate expires,
// reconcile again instead of persisting a decision from a changed input set.
func predictionInputsDeadline(reports []api.ModelStatus) *time.Time {
	var earliest *time.Time
	for _, report := range reports {
		if report.ForecastReplicas == nil || report.ExpiresAt == nil {
			continue
		}
		if earliest == nil || report.ExpiresAt.Time.Before(*earliest) {
			deadline := report.ExpiresAt.Time
			earliest = &deadline
		}
	}
	return earliest
}

// expirePredictionReports updates health after a write-time expiry. Other
// still-usable forecasts remain visible even though this decision was rejected.
func expirePredictionReports(reports []api.ModelStatus, now time.Time) bool {
	available := false
	for i := range reports {
		report := &reports[i]
		if report.ForecastReplicas == nil {
			continue
		}
		if report.ExpiresAt != nil && !now.Before(report.ExpiresAt.Time) {
			report.State = "Expired"
			report.ForecastReplicas = nil
		} else {
			available = true
		}
	}
	return available
}

// timedDecisionRequeue wakes for an already-known trajectory boundary rather
// than adding a full sync period of avoidable scheduling delay. Reconciliation
// still reloads current identity/history and obeys cooldown and rate policies.
func timedDecisionRequeue(instance *api.HPAPlus, reports []api.ModelStatus, now time.Time, regular time.Duration) time.Duration {
	names := map[string]bool{}
	for _, model := range instance.Spec.Models {
		if model.PredictivePolicy != nil {
			names[model.Name] = true
		}
	}
	for _, report := range reports {
		if !names[report.Name] || report.ForecastReplicas == nil || report.ExpiresAt == nil {
			continue
		}
		delay := report.ExpiresAt.Sub(now)
		if delay < time.Second {
			delay = time.Second
		}
		if delay < regular {
			regular = delay
		}
	}
	return regular
}

func usesExtendedTimedInterval(instance *api.HPAPlus, model *api.Model) bool {
	return model.PredictivePolicy != nil && model.PredictionMaxAge != nil
}
