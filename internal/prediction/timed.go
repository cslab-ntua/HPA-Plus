package prediction

import (
	"fmt"
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
	"math"
	"math/big"
	"time"
)

// TimedReplicas applies each trajectory point from its At, which is the
// predicted sample time. Samples are stamped at the end of their window, so
// this applies each forecast one sampling step after the window it predicts.
// ForecastWindows with TimedWindowReplicas applies it to that window instead.
func TimedReplicas(points []api.CPUForecastPoint, policy api.PredictivePolicy, capacity Capacity, requested int32, now time.Time) (int32, time.Time, error) {
	return TimedWindowReplicas(points, policy, capacity, requested, now)
}

// ForecastWindows moves a trajectory back by one sampling step, from predicted
// sample times to the windows those samples measure. The step is the last
// point's length: an earlier, already-issued point may have been shortened.
func ForecastWindows(points []api.CPUForecastPoint) []api.CPUForecastPoint {
	if len(points) == 0 {
		return nil
	}
	lastAt, _ := time.Parse(time.RFC3339Nano, points[len(points)-1].At)
	lastUntil, _ := time.Parse(time.RFC3339Nano, points[len(points)-1].Until)
	step := lastUntil.Sub(lastAt)
	out := make([]api.CPUForecastPoint, 0, len(points))
	for _, p := range points {
		at, _ := time.Parse(time.RFC3339Nano, p.At)
		until, _ := time.Parse(time.RFC3339Nano, p.Until)
		p.At, p.Until = at.Add(-step).Format(time.RFC3339Nano), until.Add(-step).Format(time.RFC3339Nano)
		out = append(out, p)
	}
	return out
}

// TimedWindowReplicas uses only the current window and impending scale-up demand.
// Missing current windows hold requested capacity; future decreases cannot
// remove capacity early. No observed CPU/reactive recommendation is added.
// The returned deadline invalidates a decision at the next window/lead boundary.
func TimedWindowReplicas(points []api.CPUForecastPoint, policy api.PredictivePolicy, capacity Capacity, requested int32, now time.Time) (int32, time.Time, error) {
	if err := algorithm.ValidateForecastTrajectory(points); err != nil {
		return 0, time.Time{}, err
	}
	tolerance := policy.EffectiveTolerance()
	if requested < 0 || policy.ScaleUpLeadSeconds < 0 || policy.ScaleUpLeadSeconds > 3600 || math.IsNaN(tolerance) || math.IsInf(tolerance, 0) || tolerance < 0 || tolerance > .2 {
		return 0, time.Time{}, fmt.Errorf("invalid timed decision policy")
	}
	lead := time.Duration(policy.ScaleUpLeadSeconds) * time.Second
	currentKnown := false
	demand := int64(0)
	var boundary time.Time
	consider := func(t time.Time) {
		if t.After(now) && (boundary.IsZero() || t.Before(boundary)) {
			boundary = t
		}
	}
	for _, p := range points {
		at, _ := time.Parse(time.RFC3339Nano, p.At)
		until, _ := time.Parse(time.RFC3339Nano, p.Until)
		consider(at)
		consider(until)
		consider(at.Add(-lead))
		if !now.Before(at) && now.Before(until) {
			currentKnown = true
			if p.CPU > demand {
				demand = p.CPU
			}
		}
		if at.After(now) && !at.After(now.Add(lead)) && p.CPU > demand {
			demand = p.CPU
		}
	}
	result, err := ResolveDemand(DemandResult{Demand: Demand{Kind: CPUKind(), Value: demand}}, Observation{Kind: CPUKind(), Capacity: capacity})
	if err != nil {
		return 0, time.Time{}, err
	}
	replicas := result.Replicas
	if !currentKnown && replicas < requested {
		replicas = requested
	}
	// Suppress small upward rounding changes around CURRENT requested capacity.
	// Do not divide every forecast by (1+tolerance), which would change the target.
	if replicas > requested && requested > 0 && tolerance > 0 {
		numerator := new(big.Int).Mul(big.NewInt(demand), big.NewInt(100))
		denominator := new(big.Int).Mul(big.NewInt(capacity.RequestPerPod), big.NewInt(int64(capacity.TargetPercentage)))
		denominator.Mul(denominator, big.NewInt(int64(requested)))
		ratio := new(big.Rat).SetFrac(numerator, denominator)
		limit := new(big.Rat).SetFloat64(1 + tolerance)
		if ratio.Cmp(limit) <= 0 {
			replicas = requested
		}
	}
	return replicas, boundary, nil
}

// MergeCurrentInterval keeps at most one already-issued current estimate until
// the first new forecast takes effect. Its original expiry is never extended.
// Every future point is replaced by the latest trajectory. This avoids either
// prematurely using a future decrease or holding forever when each new forecast
// starts one sampling interval ahead of the current clock.
func MergeCurrentInterval(fresh []api.CPUForecastPoint, old *api.CachedPrediction, fingerprint string, now time.Time) []api.CPUForecastPoint {
	out := append([]api.CPUForecastPoint(nil), fresh...)
	if len(fresh) == 0 || old == nil || old.Fingerprint != fingerprint || !now.Before(old.ExpiresAt.Time) || algorithm.ValidateForecastTrajectory(old.Trajectory) != nil {
		return out
	}
	first, _ := time.Parse(time.RFC3339Nano, fresh[0].At)
	for _, p := range old.Trajectory {
		at, _ := time.Parse(time.RFC3339Nano, p.At)
		until, _ := time.Parse(time.RFC3339Nano, p.Until)
		if until.After(first) {
			until = first
		}
		if until.After(old.ExpiresAt.Time) {
			until = old.ExpiresAt.Time
		}
		if !at.After(now) && now.Before(until) {
			p.Until = until.Format(time.RFC3339Nano)
			return append([]api.CPUForecastPoint{p}, out...)
		}
	}
	return out
}

// TrajectoryExpiry validates a fresh uniform prediction grid and returns the end
// of its last interval. A one-step forecast must be usable at its timestamp;
// the caller still caps this deadline by the explicitly configured maximum age.
func TrajectoryExpiry(points []api.CPUForecastPoint, consumed time.Time, horizon time.Duration) (time.Time, error) {
	if err := algorithm.ValidateForecastTrajectory(points); err != nil {
		return time.Time{}, err
	}
	if len(points) > algorithm.MaxTrajectoryPoints || consumed.IsZero() || horizon <= 0 {
		return time.Time{}, fmt.Errorf("invalid fresh trajectory")
	}
	first, _ := time.Parse(time.RFC3339Nano, points[0].At)
	step := first.Sub(consumed)
	if step <= 0 || step > 24*time.Hour {
		return time.Time{}, fmt.Errorf("invalid trajectory cadence")
	}
	count := (horizon-1)/step + 1
	if int64(count) != int64(len(points)) {
		return time.Time{}, fmt.Errorf("trajectory does not cover requested horizon")
	}
	var end time.Time
	for i, p := range points {
		at, _ := time.Parse(time.RFC3339Nano, p.At)
		until, _ := time.Parse(time.RFC3339Nano, p.Until)
		if !at.Equal(consumed.Add(time.Duration(i+1)*step)) || !until.Equal(at.Add(step)) {
			return time.Time{}, fmt.Errorf("invalid fresh trajectory grid")
		}
		end = until
	}
	return end, nil
}
