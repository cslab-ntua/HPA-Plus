package prediction

import (
	"context"
	"fmt"
	"github.com/cslab-ntua/HPA-Plus/internal/observability"
	"math"
	"math/big"
	ctrlLog "sigs.k8s.io/controller-runtime/pkg/log"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

// MetricKind is a dimension, not an arbitrary label. A new kind requires its own
// collection, history and capacity semantics before a model can advertise it.
type MetricKind struct {
	Source string
	Unit   string
}

func CPUKind() MetricKind { return MetricKind{Source: "resource.cpu", Unit: "millicores"} }

// Capacity describes the conversion policy independently of a fitted model.
// Only CPU request/utilization is implemented. Memory, rates and queues must not
// silently inherit this policy merely because their observations are numeric.
type Capacity struct {
	Kind             MetricKind
	Policy           string
	RequestPerPod    int64
	TargetPercentage int32
}

// Observation carries raw demand, source identity/time and the current capacity
// of one scale target. Value is integral in Kind.Unit to retain int64 precision.
type Observation struct {
	Kind     MetricKind
	Target   string
	SourceID string
	At       time.Time
	Value    int64
	Capacity Capacity
}

type Demand struct {
	Kind   MetricKind
	Target string
	Value  int64
}
type DemandResult struct {
	Trajectory    []api.CPUForecastPoint
	Demand        Demand
	ConsumedUntil *time.Time
}
type DemandRequest struct {
	Model       *api.Model
	History     []api.TimestampedSample
	Observation Observation
}

// DemandPredicter never converts demand into replicas. The runtime owns that
// boundary; existing replica-returning methods remain compatibility wrappers.
type DemandPredicter interface {
	ForecastDemand(context.Context, DemandRequest) (DemandResult, error)
}

func ValidateDemandRequest(r DemandRequest, accepted []MetricKind) error {
	if r.Model == nil {
		return fmt.Errorf("missing model")
	}
	for _, kind := range accepted {
		if r.Observation.Kind == kind {
			return nil
		}
	}
	return fmt.Errorf("model does not accept observation source/unit")
}

func CPUObservation(model *api.Model) Observation {
	o := Observation{Kind: CPUKind(), Capacity: Capacity{Kind: CPUKind(), Policy: "cpu-request-utilization"}}
	if model != nil {
		o.Capacity.RequestPerPod = model.CPURequestPerPodMillicores
		o.Capacity.TargetPercentage = model.TargetCPUUtilizationPercentage
	}
	return o
}

// ResolveDemand fails closed on cross-target/dimension results and unsupported
// policies. ceil(demand*100/(request*target)) uses exact integer arithmetic.
func ResolveDemand(result DemandResult, observation Observation) (Result, error) {
	d, c := result.Demand, observation.Capacity
	if d.Kind != observation.Kind || d.Kind != c.Kind || d.Target != observation.Target {
		return Result{}, fmt.Errorf("forecast observation identity mismatch")
	}
	if d.Kind != CPUKind() || c.Policy != "cpu-request-utilization" {
		return Result{}, fmt.Errorf("unsupported capacity conversion")
	}
	if c.RequestPerPod <= 0 || c.TargetPercentage <= 0 {
		return Result{}, fmt.Errorf("CPU capacity requires positive request and target utilization")
	}
	out := Result{ConsumedUntil: result.ConsumedUntil, Trajectory: append([]api.CPUForecastPoint(nil), result.Trajectory...)}
	if d.Value <= 0 {
		return out, nil
	}
	numerator := new(big.Int).Mul(big.NewInt(d.Value), big.NewInt(100))
	denominator := new(big.Int).Mul(big.NewInt(c.RequestPerPod), big.NewInt(int64(c.TargetPercentage)))
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)
	if remainder.Sign() != 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() || quotient.Int64() > math.MaxInt32 {
		return Result{}, fmt.Errorf("replica forecast exceeds int32")
	}
	out.Replicas = int32(quotient.Int64())
	return out, nil
}

// PredictCPU preserves the legacy Go API without coupling fitting to capacity.
func PredictCPU(ctx context.Context, p DemandPredicter, model *api.Model, history []api.TimestampedSample) (Result, error) {
	o := CPUObservation(model)
	result, err := p.ForecastDemand(ctx, DemandRequest{Model: model, History: history, Observation: o})
	if err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := ValidateTimedDemand(model, result); err != nil {
		return Result{}, err
	}
	out, err := ResolveDemand(result, o)
	if err == nil {
		logDemand(ctx, model, result, out)
	}
	return out, err
}

func logDemand(ctx context.Context, model *api.Model, demand DemandResult, result Result) {
	fields := []interface{}{"modelName", observability.Field(model.Name), "source", demand.Demand.Kind.Source, "unit", demand.Demand.Kind.Unit, "predictedDemand", demand.Demand.Value, "predictedReplicas", result.Replicas}
	if model.Type == api.TypeXGBoost || model.Type == api.TypeLightGBM {
		fields = append(fields, "diagnosticScope", DiagnosticScope(model))
	}
	ctrlLog.FromContext(ctx).V(1).Info("Raw demand forecast ready", fields...)
}
