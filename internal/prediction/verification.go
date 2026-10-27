package prediction

import (
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
	"sort"
	"time"
)

// VerificationFor uses raw CPU, never rounded replicas. One endpoint per
// forecast keeps the detector at a consistent horizon and avoids counting a
// multi-step forecast as several independent errors. Scalar maximum aggregates
// without a trajectory have no endpoint meaning and are explicitly unsupported.
func VerificationFor(model *api.Model, result DemandResult, d *algorithm.ForecastDiagnostics, rows []api.TimestampedSample) *api.ForecastVerification {
	if model.ForecastReliability == nil || result.ConsumedUntil == nil {
		return nil
	}
	var at, until time.Time
	value := result.Demand.Value
	if len(result.Trajectory) > 0 {
		if algorithm.ValidateForecastTrajectory(result.Trajectory) != nil {
			return nil
		}
		p := result.Trajectory[len(result.Trajectory)-1]
		at, _ = time.Parse(time.RFC3339Nano, p.At)
		until, _ = time.Parse(time.RFC3339Nano, p.Until)
		value = p.CPU
	} else {
		if model.XGBoost != nil && model.XGBoost.ForecastAggregation == "maximum" {
			return nil
		}
		if d == nil {
			return nil
		}
		var err error
		at, err = time.Parse(time.RFC3339Nano, d.ForecastAt)
		if err != nil {
			return nil
		}
		var stamps []time.Time
		for _, r := range rows {
			if r.Time != nil && r.TotalCPUUsageMillicores != nil {
				stamps = append(stamps, r.Time.Time)
			}
		}
		if len(stamps) < 2 {
			return nil
		}
		sort.Slice(stamps, func(i, j int) bool { return stamps[i].Before(stamps[j]) })
		var deltas []time.Duration
		for i := 1; i < len(stamps); i++ {
			delta := stamps[i].Sub(stamps[i-1])
			if delta <= 0 {
				return nil
			}
			deltas = append(deltas, delta)
		}
		sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })
		width := deltas[len(deltas)/2]
		if len(deltas)%2 == 0 {
			width = deltas[len(deltas)/2-1] + (width-deltas[len(deltas)/2-1])/2
		}
		if width <= 0 || width > 24*time.Hour {
			return nil
		}
		until = at.Add(width)
	}
	lead := at.Sub(*result.ConsumedUntil).Milliseconds()
	if value < 0 || lead <= 0 {
		return nil
	}
	return &api.ForecastVerification{At: at.UTC().Format(time.RFC3339Nano), Until: until.UTC().Format(time.RFC3339Nano), CPU: value, LeadMilliseconds: lead}
}
