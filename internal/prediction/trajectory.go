package prediction

import (
	"fmt"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
)

// ModelTrajectoryExpiry validates the same fresh grid for every model. An
// implicit Holt-Winters horizon means one observed cadence, not controller time.
func ModelTrajectoryExpiry(model *api.Model, points []api.CPUForecastPoint, consumed time.Time) (time.Time, error) {
	var millis int
	switch model.Type {
	case api.TypeLinear:
		if model.Linear != nil {
			millis = model.Linear.LookAhead
		}
	case api.TypeArima:
		if model.Arima != nil {
			millis = model.Arima.LookAhead
		}
	case api.TypeXGBoost:
		if model.XGBoost != nil {
			millis = model.XGBoost.LookAhead
		}
	case api.TypeLightGBM:
		if model.LightGBM != nil {
			millis = model.LightGBM.LookAhead
		}
	case api.TypeHoltWinters:
		if model.HoltWinters != nil && model.HoltWinters.LookAhead == nil {
			if err := algorithm.ValidateForecastTrajectory(points); err != nil {
				return time.Time{}, err
			}
			first, _ := time.Parse(time.RFC3339Nano, points[0].At)
			return TrajectoryExpiry(points, consumed, first.Sub(consumed))
		}
		if model.HoltWinters != nil && model.HoltWinters.LookAhead != nil {
			millis = *model.HoltWinters.LookAhead
		}
	}
	if millis <= 0 || int64(millis) > int64((1<<63-1)/time.Millisecond) {
		return time.Time{}, fmt.Errorf("invalid trajectory horizon")
	}
	return TrajectoryExpiry(points, consumed, time.Duration(millis)*time.Millisecond)
}

// ValidateTimedDemand rejects absent or malformed trajectories instead of
// silently treating a scalar forecast as an estimate covering now.
func ValidateTimedDemand(model *api.Model, result DemandResult) error {
	if model.PredictivePolicy == nil {
		return nil
	}
	if result.ConsumedUntil == nil {
		return fmt.Errorf("timed forecast lacks consumed timestamp")
	}
	if err := algorithm.ValidateForecastTrajectory(result.Trajectory); err != nil {
		return err
	}
	first, _ := time.Parse(time.RFC3339Nano, result.Trajectory[0].At)
	if !first.After(*result.ConsumedUntil) {
		return fmt.Errorf("forecast trajectory must follow its consumed history")
	}
	_, err := ModelTrajectoryExpiry(model, result.Trajectory, *result.ConsumedUntil)
	return err
}

// CompleteCPUForecast gives every builtin adapter the same timed-contract checks.
func CompleteCPUForecast(model *api.Model, result DemandResult) (DemandResult, error) {
	if err := ValidateTimedDemand(model, result); err != nil {
		return DemandResult{}, err
	}
	return result, nil
}
