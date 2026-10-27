package prediction

import (
	"context"
	"fmt"
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"math"
	"time"
)

// ModelContext gives the entire forecast (including hooks and fallback attempts)
// one budget, so a failed persistent request cannot double calculationTimeout.
func ModelContext(ctx context.Context, model *api.Model, defaultMillis int) (context.Context, context.CancelFunc, error) {
	if model == nil {
		return nil, nil, fmt.Errorf("missing model")
	}
	millis := defaultMillis
	if model.CalculationTimeout != nil {
		millis = *model.CalculationTimeout
	}
	if millis <= 0 || int64(millis) > math.MaxInt64/int64(time.Millisecond) {
		return nil, nil, fmt.Errorf("invalid calculationTimeout")
	}
	bounded, cancel := context.WithTimeout(ctx, time.Duration(millis)*time.Millisecond)
	return bounded, cancel, nil
}

type ContextPredicter interface {
	GetPredictionResultContext(context.Context, *api.Model, []api.TimestampedSample) (Result, error)
}

func GetResult(ctx context.Context, predictor Predicter, model *api.Model, history []api.TimestampedSample) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if predictor, ok := predictor.(ContextPredicter); ok {
		return predictor.GetPredictionResultContext(ctx, model, history)
	}
	return predictor.GetPredictionResult(model, history)
}
