package prediction

import (
	"testing"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

func trajectory(consumed time.Time, cores ...float64) []api.CPUForecastPoint {
	var out []api.CPUForecastPoint
	for i, c := range cores {
		at := consumed.Add(time.Duration(i+1) * 30 * time.Second)
		out = append(out, api.CPUForecastPoint{At: at.Format(time.RFC3339Nano), Until: at.Add(30 * time.Second).Format(time.RFC3339Nano), CPU: int64(c * 1000)})
	}
	return out
}

// A trajectory point names a predicted sample time. CPU samples are stamped at
// the end of their window, so the point forecasts the window ending at its At.
func TestForecastWindowsApplyForecastToTheWindowItPredicts(t *testing.T) {
	consumed := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	// Samples 12:00:30, 12:01:00 and 12:01:30 measure 12:00:00-30, 12:00:30-01:00 and 12:01:00-30.
	points := trajectory(consumed, .6, 3, .6)
	tolerance := .1
	policy := api.PredictivePolicy{ScaleUpLeadSeconds: 30, Tolerance: &tolerance}
	capacity := Capacity{Kind: CPUKind(), Policy: "cpu-request-utilization", RequestPerPod: 1000, TargetPercentage: 60}
	for _, c := range []struct {
		name    string
		now     time.Time
		current int32
		want    int32
	}{
		{"scale up ahead of the busy window", consumed.Add(5 * time.Second), 1, 5},
		{"busy window", consumed.Add(35 * time.Second), 5, 5},
		{"scale in when the busy window ends", consumed.Add(65 * time.Second), 5, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, _, err := TimedWindowReplicas(ForecastWindows(points), policy, capacity, c.current, c.now)
			if err != nil || got != c.want {
				t.Fatalf("got %d %v, want %d", got, err, c.want)
			}
		})
	}
	// TimedReplicas keeps applying each point from its sample time: at the end
	// of the busy window it still holds the capacity forecast for it.
	if got, _, err := TimedReplicas(points, policy, capacity, 5, consumed.Add(65*time.Second)); err != nil || got != 5 {
		t.Fatalf("TimedReplicas timing changed: %d %v", got, err)
	}
	// A previously issued point shortened by MergeCurrentInterval shifts by the
	// trajectory's sampling step, not by its own shortened length.
	merged := append([]api.CPUForecastPoint{{At: consumed.Add(-10 * time.Second).Format(time.RFC3339Nano), Until: consumed.Add(30 * time.Second).Format(time.RFC3339Nano), CPU: 900}}, points...)
	windows := ForecastWindows(merged)
	if windows[0].At != consumed.Add(-40*time.Second).Format(time.RFC3339Nano) || windows[0].Until != consumed.Format(time.RFC3339Nano) ||
		windows[1].At != consumed.Format(time.RFC3339Nano) || windows[3].Until != consumed.Add(90*time.Second).Format(time.RFC3339Nano) {
		t.Fatalf("windows not shifted by one sampling step: %+v", windows)
	}
}
