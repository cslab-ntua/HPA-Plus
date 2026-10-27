package metrics

import (
	"fmt"

	"github.com/jthomperoo/k8shorizmetrics/v2"
	gathered "github.com/jthomperoo/k8shorizmetrics/v2/metrics"
)

type Evaluator interface {
	EvaluateSingleMetricWithOptions(*gathered.Metric, int32, float64) (int32, error)
}

// HPAEvaluator applies the resource's tolerance to every metric family. The
// dependency captures tolerance in its calculator for some paths and accepts
// it as an argument for others. Constructing a call-local calculator supplies
// both without sharing mutable settings across reconciliations.
type HPAEvaluator struct{}

func (HPAEvaluator) EvaluateSingleMetricWithOptions(metric *gathered.Metric, current int32, tolerance float64) (int32, error) {
	return k8shorizmetrics.NewEvaluator(tolerance).EvaluateSingleMetricWithOptions(metric, current, tolerance)
}

// EvaluateAvailable retains the fact that a metric failed. The legacy combined
// evaluator drops that information, which otherwise lets partial observations
// drive a downscale. The controller applies the resulting downscale guard after
// combining baseline and model candidates.
func EvaluateAvailable(evaluator Evaluator, observations []*gathered.Metric, current int32, tolerance float64) (int32, bool, error) {
	var maximum int32
	valid := 0
	var firstError error
	for _, observation := range observations {
		var value int32
		var err error
		if validationErr := ValidateMetric(observation, current); validationErr != nil {
			err = validationErr
		} else {
			value, err = evaluator.EvaluateSingleMetricWithOptions(observation, current, tolerance)
			if err == nil && value < 0 {
				err = fmt.Errorf("metric evaluation returned negative replicas")
			}
		}
		if err != nil {
			if firstError == nil {
				firstError = err
			}
			continue
		}
		if value > maximum {
			maximum = value
		}
		valid++
	}
	if valid == 0 {
		if firstError != nil {
			return 0, true, fmt.Errorf("no usable metric evaluations: %w", firstError)
		}
		return 0, true, fmt.Errorf("no metric observations")
	}
	return maximum, valid != len(observations), nil
}
