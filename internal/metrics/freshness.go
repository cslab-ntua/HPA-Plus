package metrics

import (
	"context"
	"fmt"
	"time"

	gathered "github.com/jthomperoo/k8shorizmetrics/v2/metrics"
	"github.com/jthomperoo/k8shorizmetrics/v2/metrics/podmetrics"
	autoscaling "k8s.io/api/autoscaling/v2"
)

type freshnessKey struct{}
type freshnessPolicy struct {
	maxAge time.Duration
	now    func() time.Time
}

// WithFreshness enables call-scoped validation of each raw API timestamp. This
// prevents an aggregate's oldest timestamp from hiding a missing/future member.
// A nonpositive maxAge preserves the existing absence of an age policy.
func WithFreshness(ctx context.Context, maxAge time.Duration, now func() time.Time) context.Context {
	if maxAge <= 0 {
		return ctx
	}
	if now == nil {
		now = time.Now
	}
	return context.WithValue(ctx, freshnessKey{}, freshnessPolicy{maxAge: maxAge, now: now})
}

func checkTimestamp(ctx context.Context, timestamp time.Time) error {
	policy, ok := ctx.Value(freshnessKey{}).(freshnessPolicy)
	if !ok {
		return nil
	}
	if !timestampFresh(timestamp, policy.now(), policy.maxAge) {
		return fmt.Errorf("metric timestamp is missing, in the future or older than metricMaxAge")
	}
	return nil
}

func timestampFresh(timestamp, now time.Time, maxAge time.Duration) bool {
	return !timestamp.IsZero() && !timestamp.After(now) && now.Sub(timestamp) <= maxAge
}

// FreshObservations filters whole metric sources, preserving the partial-metric
// downscale guard. It also returns the earliest expiry of retained observations
// so slow prediction/persistence work cannot turn a fresh read into a stale write.
func FreshObservations(observations []*gathered.Metric, now time.Time, maxAge time.Duration) ([]*gathered.Metric, *time.Time) {
	if maxAge <= 0 {
		return observations, nil
	}
	result := make([]*gathered.Metric, 0, len(observations))
	var validUntil *time.Time
	for _, observation := range observations {
		timestamps := observationTimestamps(observation)
		if len(timestamps) == 0 {
			continue
		}
		fresh := true
		oldest := timestamps[0]
		for _, timestamp := range timestamps {
			if !timestampFresh(timestamp, now, maxAge) {
				fresh = false
				break
			}
			if timestamp.Before(oldest) {
				oldest = timestamp
			}
		}
		if !fresh {
			continue
		}
		result = append(result, observation)
		expires := oldest.Add(maxAge)
		if validUntil == nil || expires.Before(*validUntil) {
			validUntil = &expires
		}
	}
	return result, validUntil
}

func observationTimestamps(m *gathered.Metric) []time.Time {
	if m == nil {
		return nil
	}
	var values podmetrics.MetricsInfo
	switch m.Spec.Type {
	case autoscaling.ResourceMetricSourceType:
		if m.Resource != nil {
			values = m.Resource.PodMetricsInfo
		}
	case autoscaling.PodsMetricSourceType:
		if m.Pods != nil {
			values = m.Pods.PodMetricsInfo
		}
	case autoscaling.ObjectMetricSourceType:
		if m.Object != nil {
			return []time.Time{m.Object.Timestamp}
		}
	case autoscaling.ExternalMetricSourceType:
		if m.External != nil {
			return []time.Time{m.External.Timestamp}
		}
	}
	timestamps := make([]time.Time, 0, len(values))
	for _, value := range values {
		timestamps = append(timestamps, value.Timestamp)
	}
	return timestamps
}

func oldestPodTimestamp(values podmetrics.MetricsInfo) time.Time {
	var oldest time.Time
	first := true
	for _, value := range values {
		if first || value.Timestamp.Before(oldest) {
			oldest = value.Timestamp
			first = false
		}
	}
	return oldest
}
