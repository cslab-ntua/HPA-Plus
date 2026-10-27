package prediction

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

// ParseCPUForecast accepts the legacy integer/decimal protocol, rejecting values
// that would become invalid integers. Finite negatives retain the zero-clamp policy.
func ParseCPUForecast(value, modelName string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("empty %s prediction output", modelName)
	}
	integer, err := strconv.ParseInt(value, 10, 64)
	if err == nil {
		return integer, nil
	}
	floating, floatErr := strconv.ParseFloat(value, 64)
	if floatErr != nil {
		return 0, err
	}
	if math.IsNaN(floating) || math.IsInf(floating, 0) || floating >= 0x1p63 || floating < -0x1p63 {
		return 0, fmt.Errorf("%s CPU forecast must be finite and fit int64", modelName)
	}
	return int64(math.Ceil(floating)), nil
}

// CPUForecastToReplicas computes ceil(usage * 100 / (request * utilization))
// exactly, avoiding float rounding and intermediate integer overflow.
func CPUForecastToReplicas(model *api.Model, usage int64, modelName string) (int32, error) {
	o := CPUObservation(model)
	result, err := ResolveDemand(DemandResult{Demand: Demand{Kind: CPUKind(), Value: usage}}, o)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", modelName, err)
	}
	return result.Replicas, nil
}
