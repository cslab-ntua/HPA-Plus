package metrics

import "time"

// CPUObservation captures raw demand and capacity before an evaluator applies
// missing-pod substitutions. Consumers own this snapshot; source identity is
// independent of a forecast, collection slot, and desired replica count.
type CPUObservation struct {
	MetricStatus                *StatusSnapshot
	MetricsValidUntil           *time.Time
	MetricsIncomplete           bool
	TargetIdentity              string
	SourceID                    string
	SourceTime                  time.Time
	Utilization                 *float64
	TotalUsageMillicores        *int64
	RequestPerPodMillicores     *int64
	TargetUtilizationPercentage *int32
}
