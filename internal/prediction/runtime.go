package prediction

import (
	"context"
	"fmt"
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
	ctrlLog "sigs.k8s.io/controller-runtime/pkg/log"
	"time"
)

// Request is caller-owned. Runtimes must honor cancellation and must not mutate
// the model or training window. Session identity/deadlines are carried by Model.
type Request struct {
	Model       api.Model
	History     []api.TimestampedSample
	Observation *Observation
	Adapt       bool
}

// Runtime is the controller's complete forecast, retention and lifecycle seam.
// Transport implementations can change without optional assertions in the loop.
type Runtime interface {
	Predict(context.Context, Request) (Result, error)
	Prune(*api.Model, []api.TimestampedSample) ([]api.TimestampedSample, error)
	SyncSessions(string, map[string]bool)
}

type Adapter struct{ Predictor Predicter }

func (a Adapter) Predict(ctx context.Context, request Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	ctx, diagnostics := algorithm.WithForecastDiagnostics(ctx)
	model := request.Model.DeepCopy()
	action := ""
	if request.Adapt {
		action = "Unsupported"
		if provider, ok := a.Predictor.(AdaptationProvider); ok {
			var err error
			action, err = provider.PrepareAdaptation(ctx, model)
			if err != nil {
				return Result{}, err
			}
		}
	}
	rows := make([]api.TimestampedSample, len(request.History))
	for i := range request.History {
		request.History[i].DeepCopyInto(&rows[i])
	}
	if typed, ok := a.Predictor.(DemandPredicter); ok {
		observation := CPUObservation(model)
		if request.Observation != nil {
			observation = *request.Observation
		}
		demand, err := typed.ForecastDemand(ctx, DemandRequest{Model: model, History: rows, Observation: observation})
		if err != nil {
			return Result{}, err
		}
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if err := ValidateTimedDemand(model, demand); err != nil {
			return Result{}, err
		}
		result, err := ResolveDemand(demand, observation)
		if err == nil {
			result.Diagnostics = diagnostics.Last()
			if result.Diagnostics != nil {
				d := result.Diagnostics
				if d.Model != model.Type {
					return Result{}, fmt.Errorf("forecast provenance model mismatch")
				}
				if result.ConsumedUntil == nil {
					return Result{}, fmt.Errorf("forecast provenance lacks consumed marker")
				}
				end, err := time.Parse(time.RFC3339Nano, d.HistoryEnd)
				if err != nil || !end.Equal(*result.ConsumedUntil) {
					return Result{}, fmt.Errorf("forecast provenance consumed timestamp mismatch")
				}
			}
			result.Adaptation = action
			result.Verification = VerificationFor(model, demand, result.Diagnostics, rows)
			logDemand(ctx, model, demand, result)
			if result.Diagnostics != nil {
				ctrlLog.FromContext(ctx).V(1).Info("Forecast provenance", "diagnostics", result.Diagnostics)
			}
		}
		return result, err
	}
	result, err := GetResult(ctx, a.Predictor, model, rows)
	result.Adaptation = action
	return result, err
}
func (a Adapter) Prune(model *api.Model, rows []api.TimestampedSample) ([]api.TimestampedSample, error) {
	return a.Predictor.PruneHistory(model, rows)
}
func (a Adapter) SyncSessions(prefix string, active map[string]bool) {
	if owner, ok := a.Predictor.(interface{ SyncSessions(string, map[string]bool) }); ok {
		owner.SyncSessions(prefix, active)
	}
}
