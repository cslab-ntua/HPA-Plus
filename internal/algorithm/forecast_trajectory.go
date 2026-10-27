package algorithm

import (
	"context"
	"encoding/json"
	"fmt"
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	strictjson "sigs.k8s.io/json"
	"sync"
	"time"
)

// MaxTrajectoryPoints bounds wire and persisted forecasts independently of horizon.
const MaxTrajectoryPoints = 32

func ValidateForecastTrajectory(points []api.CPUForecastPoint) error {
	if len(points) < 1 || len(points) > MaxTrajectoryPoints+1 {
		return fmt.Errorf("invalid trajectory length")
	}
	var previous time.Time
	for i, p := range points {
		at, e1 := time.Parse(time.RFC3339Nano, p.At)
		until, e2 := time.Parse(time.RFC3339Nano, p.Until)
		if e1 != nil || e2 != nil || at.IsZero() || !until.After(at) || until.Sub(at) > 24*time.Hour || len(p.At) > 40 || len(p.Until) > 40 || p.CPU < 0 || (i > 0 && at.Before(previous)) {
			return fmt.Errorf("invalid trajectory point %d", i)
		}
		previous = until
	}
	return nil
}

func DecodeForecastTrajectory(raw json.RawMessage) ([]api.CPUForecastPoint, error) {
	var wire []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	for _, p := range wire {
		for _, key := range []string{"at", "until", "cpu"} {
			if len(p[key]) == 0 || string(p[key]) == "null" {
				return nil, fmt.Errorf("missing trajectory %s", key)
			}
		}
	}
	var points []api.CPUForecastPoint
	issues, err := strictjson.UnmarshalStrict(raw, &points, strictjson.DisallowDuplicateFields, strictjson.DisallowUnknownFields)
	if err != nil {
		return nil, err
	}
	if len(issues) > 0 {
		return nil, issues[0]
	}
	if err = ValidateForecastTrajectory(points); err != nil {
		return nil, err
	}
	return points, nil
}

type trajectoryKey struct{}
type TrajectoryCapture struct {
	mu     sync.Mutex
	points []api.CPUForecastPoint
}

func WithForecastTrajectory(ctx context.Context) (context.Context, *TrajectoryCapture) {
	c := &TrajectoryCapture{}
	return context.WithValue(ctx, trajectoryKey{}, c), c
}
func (c *TrajectoryCapture) Last() []api.CPUForecastPoint {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]api.CPUForecastPoint(nil), c.points...)
}
func RecordForecastTrajectory(ctx context.Context, points []api.CPUForecastPoint) {
	if c, ok := ctx.Value(trajectoryKey{}).(*TrajectoryCapture); ok {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.points = append([]api.CPUForecastPoint(nil), points...)
	}
}
