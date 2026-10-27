package workloaddemand

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

// TimeInterval describes real measurement support, not a relabelled sample.
type TimeInterval struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// PodLifetime is the runtime inventory's conservative possible-running support.
// An uncertain exit tail remains inside Intervals; it is never assumed idle.
type PodLifetime struct {
	Known      bool           `json:"known"`
	Intervals  []TimeInterval `json:"intervals"`
	Reason     string         `json:"reason,omitempty"`
	Boundaries []time.Time    `json:"boundaries,omitempty"`
}

type PodTrafficCoverage struct {
	PodIdentity
	ExpectedSeconds float64        `json:"expectedSeconds"`
	CoveredSeconds  float64        `json:"coveredSeconds"`
	Intervals       []TimeInterval `json:"intervals,omitempty"`
	Gaps            []TimeInterval `json:"gaps,omitempty"`
	Reason          string         `json:"reason,omitempty"`
}

type counterSegment struct {
	TimeInterval
	Count                   float64
	NativeFrom, NativeUntil time.Time
}

type counterSupport struct {
	Segments []counterSegment
	Covered  []TimeInterval
	Expected []TimeInterval
	Known    bool
	Reason   string
	Native   []PodCounterInterval
	SourceAt time.Time
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func intervalSeconds(values []TimeInterval) float64 {
	var seconds float64
	for _, value := range values {
		seconds += value.To.Sub(value.From).Seconds()
	}
	return seconds
}
func unionIntervals(values []TimeInterval) []TimeInterval {
	values = append([]TimeInterval(nil), values...)
	sort.Slice(values, func(i, j int) bool { return values[i].From.Before(values[j].From) })
	var out []TimeInterval
	for _, value := range values {
		if !value.To.After(value.From) {
			continue
		}
		if len(out) == 0 || value.From.After(out[len(out)-1].To) {
			out = append(out, value)
		} else if value.To.After(out[len(out)-1].To) {
			out[len(out)-1].To = value.To
		}
	}
	return out
}
func intersectIntervals(a, b []TimeInterval) []TimeInterval {
	var out []TimeInterval
	for _, x := range a {
		for _, y := range b {
			from, to := later(x.From, y.From), earlier(x.To, y.To)
			if to.After(from) {
				out = append(out, TimeInterval{from, to})
			}
		}
	}
	return unionIntervals(out)
}
func subtractIntervals(a, b []TimeInterval) []TimeInterval {
	out := unionIntervals(a)
	for _, remove := range unionIntervals(b) {
		var next []TimeInterval
		for _, value := range out {
			if !remove.To.After(value.From) || !value.To.After(remove.From) {
				next = append(next, value)
				continue
			}
			if remove.From.After(value.From) {
				next = append(next, TimeInterval{value.From, earlier(value.To, remove.From)})
			}
			if remove.To.Before(value.To) {
				next = append(next, TimeInterval{later(value.From, remove.To), value.To})
			}
		}
		out = next
	}
	return out
}
func expectedLifetime(scope Scope, pod PodIdentity, from, to time.Time) ([]TimeInterval, bool, string) {
	if scope.Lifetimes == nil {
		return []TimeInterval{{from, to}}, true, ""
	}
	life, ok := scope.Lifetimes[pod.UID]
	if !ok || !life.Known {
		return []TimeInterval{{from, to}}, false, "runtime lifetime inventory unavailable"
	}
	var intervals []TimeInterval
	for _, interval := range life.Intervals {
		if interval.To.IsZero() {
			interval.To = to
		}
		intervals = append(intervals, interval)
	}
	return intersectIntervals(unionIntervals(intervals), []TimeInterval{{from, to}}), true, life.Reason
}

func pairWithinRuntime(scope Scope, pod PodIdentity, from, to time.Time) bool {
	if scope.Lifetimes == nil {
		return true
	}
	life := scope.Lifetimes[pod.UID]
	for _, boundary := range life.Boundaries {
		if boundary.After(from) && boundary.Before(to) {
			return false
		}
	}
	for _, interval := range life.Intervals {
		if !from.Before(interval.From) && (interval.To.IsZero() || !to.After(interval.To)) {
			return true
		}
	}
	return false
}

// counterSupports integrates only measured adjacent counter pairs. Boundary
// averages use their actual interval and never cross a reset or missing read.
func (r *TrafficReader) counterSupports(ctx context.Context, scope Scope, t api.TrafficProviderConfig, metric string, from, to time.Time, age time.Duration) (map[PodIdentity]counterSupport, error) {
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	queryTo := earlier(to.Add(age), now)
	series, err := r.query(ctx, scope, t, metric, from.Add(-age), queryTo)
	if err != nil {
		return nil, err
	}
	return supportsFromSeries(scope, t, series, from, to, age), nil
}

func supportsFromSeries(scope Scope, t api.TrafficProviderConfig, series []promSeries, from, to time.Time, age time.Duration) map[PodIdentity]counterSupport {
	_, podLabel, uidLabel := labels(t)
	grouped := map[PodIdentity][]promSeries{}
	for _, value := range series {
		pod := PodIdentity{value.Metric[podLabel], value.Metric[uidLabel]}
		grouped[pod] = append(grouped[pod], value)
	}
	out := map[PodIdentity]counterSupport{}
	for _, pod := range scope.Pods {
		expected, known, reason := expectedLifetime(scope, pod, from, to)
		value := counterSupport{Expected: expected, Known: known, Reason: reason}
		if !known {
			out[pod] = value
			continue
		}
		if len(expected) == 0 {
			out[pod] = value
			continue
		}
		rows := grouped[pod]
		if len(rows) == 0 {
			value.Reason = "counter series unavailable for active runtime"
			out[pod] = value
			continue
		}
		common := append([]TimeInterval(nil), expected...)
		// Native pairs shared by every counter label can be matched exactly to
		// CPU. A lazily absent category is not silently assumed to be zero.
		type pairKey struct{ from, to time.Time }
		pairs := map[pairKey]float64{}
		pairLabels := map[pairKey]int{}
		for _, row := range rows {
			var support []TimeInterval
			seenPairs := map[pairKey]bool{}
			for i := 1; i < len(row.Samples); i++ {
				a, b := row.Samples[i-1], row.Samples[i]
				if !b.At.After(a.At) || a.Value < 0 || b.Value < a.Value || b.At.Sub(a.At) > 2*age {
					value.Reason = "counter reset, duplicate or missing native interval"
					continue
				}
				if !pairWithinRuntime(scope, pod, a.At, b.At) {
					value.Reason = "native counter pair crosses runtime boundary"
					continue
				}
				for _, life := range expected {
					left, right := later(a.At, life.From), earlier(b.At, life.To)
					if !right.After(left) {
						continue
					}
					segment := counterSegment{TimeInterval: TimeInterval{left, right}, Count: (b.Value - a.Value) * right.Sub(left).Seconds() / b.At.Sub(a.At).Seconds(), NativeFrom: a.At, NativeUntil: b.At}
					value.Segments = append(value.Segments, segment)
					support = append(support, segment.TimeInterval)
					if b.At.After(value.SourceAt) {
						value.SourceAt = b.At
					}
					if !a.At.Before(life.From) && !b.At.After(life.To) {
						key := pairKey{a.At, b.At}
						pairs[key] += b.Value - a.Value
						if !seenPairs[key] {
							pairLabels[key]++
							seenPairs[key] = true
						}
					}
				}
			}
			common = intersectIntervals(common, unionIntervals(support))
		}
		value.Covered = common
		for pair, count := range pairs {
			if pairLabels[pair] == len(rows) {
				value.Native = append(value.Native, PodCounterInterval{Pod: pod.Name, UID: pod.UID, Start: pair.from, End: pair.to, CompletedRequests: count})
			}
		}
		sort.Slice(value.Native, func(i, j int) bool { return value.Native[i].Start.Before(value.Native[j].Start) })
		out[pod] = value
	}
	return out
}

func fleetCounterRate(scope Scope, values map[PodIdentity]counterSupport, from, to time.Time) (float64, float64, bool, time.Time, []PodTrafficCoverage) {
	common := []TimeInterval{{from, to}}
	var details []PodTrafficCoverage
	var source time.Time
	for _, pod := range scope.Pods {
		value, exists := values[pod]
		if !exists || !value.Known {
			common = nil
			details = append(details, PodTrafficCoverage{PodIdentity: pod, ExpectedSeconds: to.Sub(from).Seconds(), Gaps: []TimeInterval{{From: from, To: to}}, Reason: "runtime inventory unavailable"})
			continue
		}
		gaps := subtractIntervals(value.Expected, value.Covered)
		common = subtractIntervals(common, gaps)
		details = append(details, PodTrafficCoverage{PodIdentity: pod, ExpectedSeconds: intervalSeconds(value.Expected), CoveredSeconds: intervalSeconds(value.Covered), Intervals: value.Covered, Gaps: gaps, Reason: value.Reason})
		if value.SourceAt.After(source) {
			source = value.SourceAt
		}
	}
	seconds := intervalSeconds(common)
	coverage := seconds / to.Sub(from).Seconds()
	if seconds <= 0 {
		return 0, coverage, false, source, details
	}
	var count float64
	averaged := false
	for _, pod := range scope.Pods {
		for _, segment := range values[pod].Segments {
			for _, covered := range intersectIntervals([]TimeInterval{segment.TimeInterval}, common) {
				count += segment.Count * covered.To.Sub(covered.From).Seconds() / segment.To.Sub(segment.From).Seconds()
				if !covered.From.Equal(segment.NativeFrom) || !covered.To.Equal(segment.NativeUntil) {
					averaged = true
				}
			}
		}
	}
	return count / seconds, math.Min(1, coverage), averaged, source, details
}

func mergeNativeIntervals(values []PodCounterInterval) []PodCounterInterval {
	var out []PodCounterInterval
	for _, value := range values {
		if len(out) > 0 && out[len(out)-1].UID == value.UID && out[len(out)-1].End.Equal(value.Start) {
			out[len(out)-1].End = value.End
			out[len(out)-1].CompletedRequests += value.CompletedRequests
		} else {
			out = append(out, value)
		}
	}
	return out
}

func nativeCompletionSupport(scope Scope, values map[PodIdentity]counterSupport) ([]PodCounterInterval, []PodCounterInterval, float64, float64, float64, string) {
	var native []PodCounterInterval
	var expected, present int
	var expectedSeconds, measuredSeconds float64
	var reasons []string
	for _, pod := range scope.Pods {
		value, exists := values[pod]
		if !exists || !value.Known {
			expectedSeconds += intervalSeconds(value.Expected)
			expected++
			reasons = append(reasons, pod.Name+": unknown runtime inventory")
			continue
		}
		expectedSeconds += intervalSeconds(value.Expected)
		if len(value.Expected) == 0 {
			continue
		}
		expected++
		if len(value.Native) == 0 {
			reasons = append(reasons, pod.Name+": no complete native completion interval")
			continue
		}
		present++
		for _, interval := range value.Native {
			native = append(native, interval)
			measuredSeconds += interval.End.Sub(interval.Start).Seconds()
		}
	}
	coverage := 1.
	if expected > 0 {
		coverage = float64(present) / float64(expected)
	}
	return mergeNativeIntervals(native), native, coverage, expectedSeconds, measuredSeconds, fmt.Sprint(reasons)
}
