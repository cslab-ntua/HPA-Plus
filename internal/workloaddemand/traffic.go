package workloaddemand

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

type PodIdentity struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
}
type Scope struct {
	Namespace string                 `json:"namespace"`
	Pods      []PodIdentity          `json:"pods"`
	Lifetimes map[string]PodLifetime `json:"lifetimes,omitempty"`
}
type FeatureValue struct {
	Value             *float64      `json:"value"`
	SourceAt          time.Time     `json:"sourceAt"`
	ReceivedAt        time.Time     `json:"receivedAt"`
	Unit              string        `json:"unit"`
	MissingReason     string        `json:"missingReason,omitempty"`
	MeasurementWindow *TimeInterval `json:"measurementWindow,omitempty"`
}
type PodCounterInterval struct {
	Pod               string    `json:"pod"`
	UID               string    `json:"uid"`
	Start             time.Time `json:"start"`
	End               time.Time `json:"end"`
	CompletedRequests float64   `json:"completedRequests"`
}
type TrafficWindow struct {
	Start                   time.Time               `json:"start"`
	End                     time.Time               `json:"end"`
	AvailableAt             time.Time               `json:"availableAt"`
	ArrivalsPerSecond       float64                 `json:"arrivalsPerSecond"`
	CompletedRequests       float64                 `json:"completedRequests"`
	PerPodIntervals         []PodCounterInterval    `json:"perPodIntervals"`
	Features                map[string]FeatureValue `json:"features"`
	ArrivalCoverage         float64                 `json:"arrivalCoverage"`
	Coverage                float64                 `json:"coverage"`
	Qualified               bool                    `json:"qualified"`
	Reason                  string                  `json:"reason,omitempty"`
	ArrivalBoundaryAveraged bool                    `json:"arrivalBoundaryAveraged"`
	ArrivalPodCoverage      []PodTrafficCoverage    `json:"arrivalPodCoverage,omitempty"`
	ExpectedPodSeconds      float64                 `json:"expectedPodSeconds"`
	NativePodSeconds        float64                 `json:"nativePodSeconds"`
	NativeSupportFraction   float64                 `json:"nativeSupportFraction"`
	PerPodNativeIntervals   []PodCounterInterval    `json:"-"`
	ArrivalIntervals        []ArrivalInterval       `json:"arrivalIntervals,omitempty"`
}

// ArrivalInterval is a separately measured rate on a shorter counter window.
// It never replaces the original thirty-second mean or fills its gaps.
type ArrivalInterval struct {
	From             time.Time `json:"from"`
	To               time.Time `json:"to"`
	AvailableAt      time.Time `json:"availableAt"`
	Rate             *float64  `json:"rate"`
	Coverage         float64   `json:"coverage"`
	BoundaryAveraged bool      `json:"boundaryAveraged"`
}

type TrafficReader struct {
	URL    string
	Client *http.Client
	Now    func() time.Time
}

var metricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ValidateConfig prevents arbitrary PromQL and ambiguous workload boundaries.
func ValidateConfig(config api.RequestDemandConfig) error {
	switch config.ForecastIntervalSeconds {
	case 0, 5, 10, 15, 30:
	default:
		return errors.New("request forecast interval must be 5, 10, 15 or 30 seconds")
	}
	switch config.ForecastTarget {
	case "", api.RequestDemandTargetArrivals:
	case api.RequestDemandTargetReconstructedCPU:
		// Reconstructed demand exists only per closed thirty-second window.
		if config.ForecastIntervalSeconds != 0 && config.ForecastIntervalSeconds != 30 {
			return errors.New("reconstructedCPU forecasts use the thirty-second grid")
		}
	default:
		return errors.New("request forecast target must be arrivals or reconstructedCPU")
	}
	t := config.Traffic
	if t.Provider != "prometheus" || t.ArrivalSemantics != "started" || t.CompletionSemantics != "finished" || (t.RetrySemantics != "attempts" && t.RetrySemantics != "original") {
		return errors.New("requestDemand requires prometheus, explicit started/finished counters and attempts/original retry semantics")
	}
	if t.ArrivalsMetric == t.CompletionsMetric {
		return errors.New("arrival and completion counters must be distinct")
	}
	for _, m := range []string{t.ArrivalsMetric, t.CompletionsMetric} {
		if !metricName.MatchString(m) {
			return fmt.Errorf("invalid required counter name %q", m)
		}
	}
	for _, m := range []string{t.ErrorsMetric, t.OutcomesMetric, t.LatencyBucketMetric} {
		if m != "" && !metricName.MatchString(m) {
			return fmt.Errorf("invalid metric name %q", m)
		}
	}
	ns, pod, uid := labels(t)
	if !labelName.MatchString(ns) || !labelName.MatchString(pod) || !labelName.MatchString(uid) || ns == pod || ns == uid || pod == uid {
		return errors.New("namespace, pod and pod UID labels must be distinct valid label names")
	}
	if len(t.AdditionalLabels) > 16 {
		return errors.New("at most sixteen additional labels are supported")
	}
	for k := range t.AdditionalLabels {
		if len(t.AdditionalLabels[k]) > 256 {
			return errors.New("additional label values must be at most 256 bytes")
		}
		if !labelName.MatchString(k) || k == ns || k == pod || k == uid || k == "le" {
			return errors.New("additional labels cannot override workload scope or histogram boundary")
		}
	}
	if t.MaxAgeSeconds < 0 || t.MaxAgeSeconds > 30 {
		return errors.New("traffic maxAgeSeconds must be between 1 and 30, or omitted")
	}
	if len(config.ExtraMetrics) > 16 {
		return errors.New("at most sixteen optional metrics are supported")
	}
	names := map[string]bool{"cpu_usage": true, "memory_usage": true, "cpu_throttled": true, "cpu_pressure": true, "requested_replicas": true, "ready_replicas": true, "running_replicas": true, "cpu_request_per_pod": true}
	for _, m := range config.ExtraMetrics {
		if !labelName.MatchString(m.Name) || names[m.Name] || strings.HasPrefix(m.Name, "traffic_") {
			return errors.New("extra metric names must be unique valid names outside reserved traffic_ prefix")
		}
		names[m.Name] = true
		if !metricName.MatchString(m.Metric) || m.Unit == "" || len(m.Unit) > 64 || (m.Kind != "gauge" && m.Kind != "counter") || m.MaxAgeSeconds < 0 || m.MaxAgeSeconds > 120 {
			return errors.New("extra metrics require a metric name, unit, gauge/counter kind and at most 120 seconds age")
		}
	}
	return nil
}
func labels(t api.TrafficProviderConfig) (string, string, string) {
	ns, pod, uid := t.NamespaceLabel, t.PodLabel, t.PodUIDLabel
	if ns == "" {
		ns = "namespace"
	}
	if pod == "" {
		pod = "pod"
	}
	if uid == "" {
		uid = "pod_uid"
	}
	return ns, pod, uid
}

// ReadWindow queries native counter samples, not query_range evaluation timestamps.
// Every selector is scoped server-side to resolved namespace, pod names and UIDs.
// The caller must integrate CPU over PerPodIntervals before updating its estimator.
func (r *TrafficReader) ReadWindow(ctx context.Context, scope Scope, config api.RequestDemandConfig, from, to time.Time) (TrafficWindow, error) {
	out := TrafficWindow{Start: from, End: to, Features: map[string]FeatureValue{}}
	if err := ValidateConfig(config); err != nil {
		return out, err
	}
	if scope.Namespace == "" || len(scope.Pods) == 0 || len(scope.Pods) > 128 || !to.After(from) || to.Sub(from) > time.Minute {
		return out, errors.New("invalid or unbounded workload window")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	if to.After(now()) {
		return out, errors.New("future traffic windows are not readable")
	}
	maxAge := time.Duration(config.Traffic.MaxAgeSeconds) * time.Second
	if maxAge == 0 {
		maxAge = 3 * time.Second
	}
	arrival, err := r.counterSupports(ctx, scope, config.Traffic, config.Traffic.ArrivalsMetric, from, to, maxAge)
	if err != nil {
		return out, fmt.Errorf("arrival counter: %w", err)
	}
	completed, completionErr := r.counterSupports(ctx, scope, config.Traffic, config.Traffic.CompletionsMetric, from, to, maxAge)
	out.AvailableAt = now()
	arrivalRate, coverage, averaged, arrivalSource, details := fleetCounterRate(scope, arrival, from, to)
	out.ArrivalsPerSecond, out.ArrivalCoverage, out.ArrivalBoundaryAveraged, out.ArrivalPodCoverage = arrivalRate, coverage, averaged, details
	if seconds := config.ForecastIntervalSeconds; seconds > 0 && seconds < 30 {
		step := time.Duration(seconds) * time.Second
		for left := from; !left.Add(step).After(to); left = left.Add(step) {
			right := left.Add(step)
			rate, covered, boundary, _, _ := fleetCounterRate(scope, arrival, left, right)
			point := ArrivalInterval{From: left, To: right, Coverage: covered, BoundaryAveraged: boundary}
			if covered >= .95 {
				value := rate
				point.Rate = &value
			}
			out.ArrivalIntervals = append(out.ArrivalIntervals, point)
		}
	}
	out.PerPodIntervals, out.PerPodNativeIntervals, out.Coverage, out.ExpectedPodSeconds, out.NativePodSeconds, out.Reason = nativeCompletionSupport(scope, completed)
	if out.ExpectedPodSeconds > 0 {
		out.NativeSupportFraction = out.NativePodSeconds / out.ExpectedPodSeconds
	}
	for _, interval := range out.PerPodIntervals {
		out.CompletedRequests += interval.CompletedRequests
	}
	out.Qualified = completionErr == nil && out.Coverage == 1 && len(out.PerPodIntervals) > 0
	if completionErr != nil {
		out.Reason = completionErr.Error()
	}
	feature := func(name, unit string, value *float64, source time.Time, reason string) {
		if source.IsZero() {
			source = to
		}
		out.Features[name] = FeatureValue{Value: value, SourceAt: source, ReceivedAt: now(), Unit: unit, MissingReason: reason,
			MeasurementWindow: &TimeInterval{From: from, To: to}}
	}
	if coverage >= .95 {
		v := arrivalRate
		feature("traffic_arrivals", "requests/second", &v, arrivalSource, "")
	} else {
		feature("traffic_arrivals", "requests/second", nil, to, "incomplete fleet arrival support")
	}
	completionRate, completionCoverage, _, completionSource, _ := fleetCounterRate(scope, completed, from, to)
	if completionErr == nil && completionCoverage >= .95 {
		feature("traffic_completions", "requests/second", &completionRate, completionSource, "")
	} else {
		feature("traffic_completions", "requests/second", nil, to, "incomplete fleet completion support")
	}
	if ratio, source, reason := matchedBacklogRatio(scope, arrival, completed, completionErr, from, to); ratio != nil {
		feature("traffic_backlog_ratio", "ratio", ratio, source, "")
	} else {
		feature("traffic_backlog_ratio", "ratio", nil, to, reason)
	}
	if config.Traffic.ErrorsMetric != "" {
		counters, e := r.counterSupports(ctx, scope, config.Traffic, config.Traffic.ErrorsMetric, from, to, maxAge)
		v, cov, _, source, _ := fleetCounterRate(scope, counters, from, to)
		// This is a rate in errors/second, not the fraction of finished outcomes.
		feature("traffic_error_rate", "ratio", nil, to, "all-outcome counter is not configured")
		if e != nil || cov < .95 {
			feature("traffic_errors", "errors/second", nil, to, "error counter unavailable")
			feature("traffic_error_rate", "ratio", nil, to, "error counter unavailable")
		} else {
			feature("traffic_errors", "errors/second", &v, source, "")
			if config.Traffic.OutcomesMetric != "" {
				outcomes, readErr := r.counterSupports(ctx, scope, config.Traffic, config.Traffic.OutcomesMetric, from, to, maxAge)
				if readErr != nil {
					feature("traffic_error_rate", "ratio", nil, to, "all-outcome counter unavailable")
				} else {
					ratio, ratioSource, reason := finishedOutcomeErrorFraction(scope, counters, outcomes, from, to)
					feature("traffic_error_rate", "ratio", ratio, ratioSource, reason)
				}
			}
		}
	}
	if config.Traffic.LatencyBucketMetric != "" {
		v, source, e := r.readLatency(ctx, scope, config.Traffic, from, to, maxAge)
		if e != nil {
			feature("traffic_latency_p95", "seconds", nil, to, e.Error())
		} else {
			feature("traffic_latency_p95", "seconds", &v, source, "")
		}
	}
	for _, m := range config.ExtraMetrics {
		age := time.Duration(m.MaxAgeSeconds) * time.Second
		if age == 0 {
			age = maxAge
		}
		if m.Kind == "counter" {
			values, e := r.counterSupports(ctx, scope, config.Traffic, m.Metric, from, to, age)
			v, cov, _, source, _ := fleetCounterRate(scope, values, from, to)
			if e != nil || cov < .95 {
				feature(m.Name, m.Unit, nil, to, "optional counter unavailable")
			} else {
				feature(m.Name, m.Unit, &v, source, "")
			}
		} else {
			v, source, e := r.readGauge(ctx, scope, config.Traffic, m.Metric, to, age)
			if e != nil {
				feature(m.Name, m.Unit, nil, to, e.Error())
			} else {
				feature(m.Name, m.Unit, &v, source, "")
				value := out.Features[m.Name]
				value.MeasurementWindow = nil
				out.Features[m.Name] = value
			}
		}
	}
	// Optional queries can finish later; their receipt must never be backdated.
	out.AvailableAt = now()
	for i := range out.ArrivalIntervals {
		out.ArrivalIntervals[i].AvailableAt = out.AvailableAt
	}
	for key, value := range out.Features {
		value.ReceivedAt = out.AvailableAt
		out.Features[key] = value
	}
	return out, nil
}

// backlogRatioMinimumCoverage is the share of a window that both counters
// must measure on every running pod before their ratio is reported.
const backlogRatioMinimumCoverage = .5

// matchedBacklogRatio divides arrivals by finished requests over the seconds
// both counters measured on every running pod. Unlike either rate it needs no
// full-window support: a pod that starts or stops inside the window leaves
// seconds without counts, and those seconds are left out of both counts alike.
func matchedBacklogRatio(scope Scope, arrivals, completions map[PodIdentity]counterSupport, completionErr error, from, to time.Time) (*float64, time.Time, string) {
	if completionErr != nil {
		return nil, to, "completion counter unavailable"
	}
	matchedArrivals := make(map[PodIdentity]counterSupport, len(scope.Pods))
	matchedCompletions := make(map[PodIdentity]counterSupport, len(scope.Pods))
	for _, pod := range scope.Pods {
		arrival, arrivalOK := arrivals[pod]
		completion, completionOK := completions[pod]
		if !arrivalOK || !completionOK || !arrival.Known || !completion.Known {
			return nil, to, "incomplete fleet runtime inventory"
		}
		common := intersectIntervals(arrival.Covered, completion.Covered)
		arrival.Covered, completion.Covered = common, common
		matchedArrivals[pod], matchedCompletions[pod] = arrival, completion
	}
	arrivalRate, coverage, _, arrivalSource, _ := fleetCounterRate(scope, matchedArrivals, from, to)
	completionRate, _, _, completionSource, _ := fleetCounterRate(scope, matchedCompletions, from, to)
	if coverage < backlogRatioMinimumCoverage {
		return nil, to, "too few seconds measured by both counters"
	}
	if completionRate <= 0 {
		return nil, to, "no finished requests in the measured seconds"
	}
	ratio := arrivalRate / completionRate
	return &ratio, later(arrivalSource, completionSource), ""
}

// finishedOutcomeErrorFraction compares errors with all finished outcomes on
// identical measured fleet support. Neither a processed-work counter nor a
// healthy subset is an admissible denominator. Missing/reset/stale support
// remains missing; the ratio is not clamped when the counters are inconsistent.
func finishedOutcomeErrorFraction(scope Scope, failures, outcomes map[PodIdentity]counterSupport, from, to time.Time) (*float64, time.Time, string) {
	matchedFailures := make(map[PodIdentity]counterSupport, len(scope.Pods))
	matchedOutcomes := make(map[PodIdentity]counterSupport, len(scope.Pods))
	for _, pod := range scope.Pods {
		failure, failureOK := failures[pod]
		outcome, outcomeOK := outcomes[pod]
		if !failureOK || !outcomeOK || !failure.Known || !outcome.Known {
			return nil, to, "incomplete fleet outcome support"
		}
		common := intersectIntervals(failure.Covered, outcome.Covered)
		failure.Covered, outcome.Covered = common, common
		matchedFailures[pod], matchedOutcomes[pod] = failure, outcome
	}
	failureRate, failureCoverage, _, failureSource, _ := fleetCounterRate(scope, matchedFailures, from, to)
	outcomeRate, outcomeCoverage, _, outcomeSource, _ := fleetCounterRate(scope, matchedOutcomes, from, to)
	source := later(failureSource, outcomeSource)
	if failureCoverage < .95 || outcomeCoverage < .95 {
		return nil, source, "incomplete fleet outcome support"
	}
	if outcomeRate <= 0 {
		return nil, source, "no finished outcomes in the measurement window"
	}
	if failureRate > outcomeRate {
		return nil, source, "error counter exceeds all finished outcomes"
	}
	ratio := failureRate / outcomeRate
	return &ratio, source, ""
}

type promSample struct {
	At    time.Time
	Value float64
}
type promSeries struct {
	Metric  map[string]string
	Samples []promSample
}
type counterValue struct {
	Start, End time.Time
	Count      float64
	Valid      bool
}

func totalRate(scope Scope, values map[PodIdentity]counterValue) (float64, bool) {
	v := 0.
	for _, pod := range scope.Pods {
		c, ok := values[pod]
		if !ok || !c.Valid {
			return 0, false
		}
		v += c.Count / c.End.Sub(c.Start).Seconds()
	}
	return v, true
}

func (r *TrafficReader) readCounter(ctx context.Context, scope Scope, t api.TrafficProviderConfig, metric string, from, to time.Time, age time.Duration) (map[PodIdentity]counterValue, error) {
	series, err := r.query(ctx, scope, t, metric, from.Add(-age), to)
	if err != nil {
		return nil, err
	}
	_, podLabel, uidLabel := labels(t)
	expected := map[PodIdentity]bool{}
	for _, pod := range scope.Pods {
		expected[pod] = true
	}
	result := map[PodIdentity]counterValue{}
	invalid := map[PodIdentity]bool{}
	for _, s := range series {
		pod := PodIdentity{Name: s.Metric[podLabel], UID: s.Metric[uidLabel]}
		if !expected[pod] {
			return nil, errors.New("Prometheus returned a series outside the resolved pod identity")
		}
		value, ok := counterDelta(s.Samples, from, to, age)
		if !ok {
			invalid[pod] = true
			continue
		}
		if old, exists := result[pod]; exists {
			if !old.Start.Equal(value.Start) || !old.End.Equal(value.End) {
				invalid[pod] = true
				continue
			}
			value.Count += old.Count
		}
		result[pod] = value
	}
	for pod := range invalid {
		delete(result, pod)
	}
	return result, nil
}
func counterDelta(samples []promSample, from, to time.Time, maxAge time.Duration) (counterValue, bool) {
	first, last := -1, -1
	for i, s := range samples {
		if first < 0 && !s.At.Before(from) && !s.At.After(to) {
			first = i
		}
		if !s.At.After(to) {
			last = i
		}
	}
	if first < 0 || last <= first {
		return counterValue{}, false
	}
	a, b := samples[first], samples[last]
	if a.At.Sub(from) > maxAge || to.Sub(b.At) > maxAge || !b.At.After(a.At) {
		return counterValue{}, false
	}
	if a.Value < 0 {
		return counterValue{}, false
	}
	for i := first + 1; i <= last; i++ {
		if !samples[i].At.After(samples[i-1].At) || samples[i].Value < samples[i-1].Value || samples[i].At.Sub(samples[i-1].At) > maxAge*2 {
			return counterValue{}, false
		}
	}
	return counterValue{Start: a.At, End: b.At, Count: b.Value - a.Value, Valid: true}, true
}

func (r *TrafficReader) readGauge(ctx context.Context, scope Scope, t api.TrafficProviderConfig, metric string, to time.Time, age time.Duration) (float64, time.Time, error) {
	series, e := r.query(ctx, scope, t, metric, to.Add(-age), to)
	if e != nil {
		return 0, time.Time{}, e
	}
	_, podLabel, uidLabel := labels(t)
	expected := map[PodIdentity]bool{}
	for _, pod := range scope.Pods {
		life, known, _ := expectedLifetime(scope, pod, to, to.Add(time.Nanosecond))
		if !known {
			return 0, time.Time{}, errors.New("optional gauge runtime inventory unavailable")
		}
		if len(life) > 0 {
			expected[pod] = true
		}
	}
	seen := map[PodIdentity]bool{}
	value := 0.
	source := to
	for _, s := range series {
		pod := PodIdentity{Name: s.Metric[podLabel], UID: s.Metric[uidLabel]}
		if !expected[pod] {
			continue
		}
		if len(s.Samples) == 0 {
			continue
		}
		last := s.Samples[len(s.Samples)-1]
		if to.Sub(last.At) > age || last.At.After(to) {
			return 0, time.Time{}, errors.New("optional gauge is stale")
		}
		value += last.Value
		seen[pod] = true
		if last.At.Before(source) {
			source = last.At
		}
	}
	if len(seen) != len(expected) {
		return 0, time.Time{}, errors.New("optional gauge has incomplete active pod coverage")
	}
	return value, source, nil
}
func (r *TrafficReader) readLatency(ctx context.Context, scope Scope, t api.TrafficProviderConfig, from, to time.Time, age time.Duration) (float64, time.Time, error) {
	queryTo := to.Add(age)
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	if queryTo.After(now) {
		queryTo = now
	}
	series, e := r.query(ctx, scope, t, t.LatencyBucketMetric, from.Add(-age), queryTo)
	if e != nil {
		return 0, time.Time{}, e
	}
	grouped := map[float64][]promSeries{}
	for _, s := range series {
		bound, err := strconv.ParseFloat(s.Metric["le"], 64)
		if err != nil || math.IsNaN(bound) {
			return 0, time.Time{}, errors.New("invalid histogram boundary")
		}
		grouped[bound] = append(grouped[bound], s)
	}
	buckets := map[float64]float64{}
	var source time.Time
	for bound, rows := range grouped {
		support := supportsFromSeries(scope, t, rows, from, to, age)
		rate, coverage, _, at, _ := fleetCounterRate(scope, support, from, to)
		if coverage < .95 {
			return 0, time.Time{}, errors.New("latency histogram incomplete fleet coverage")
		}
		buckets[bound] = rate
		if at.After(source) {
			source = at
		}
	}
	if buckets[math.Inf(1)] <= 0 {
		return 0, time.Time{}, errors.New("latency histogram unavailable")
	}
	keys := make([]float64, 0, len(buckets))
	for bound := range buckets {
		keys = append(keys, bound)
	}
	sort.Float64s(keys)
	target := .95 * buckets[math.Inf(1)]
	previousBound, previousCount := 0., 0.
	for _, bound := range keys {
		count := buckets[bound]
		if count < previousCount {
			return 0, time.Time{}, errors.New("nonmonotonic histogram buckets")
		}
		if count >= target {
			if math.IsInf(bound, 1) {
				return previousBound, source, nil
			}
			if count == previousCount {
				return bound, source, nil
			}
			return previousBound + (bound-previousBound)*(target-previousCount)/(count-previousCount), source, nil
		}
		previousBound, previousCount = bound, count
	}
	return 0, time.Time{}, errors.New("histogram quantile unavailable")
}

func (r *TrafficReader) query(ctx context.Context, scope Scope, t api.TrafficProviderConfig, metric string, from, to time.Time) ([]promSeries, error) {
	endpoint, e := url.Parse(r.URL)
	if e != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("trusted Prometheus URL must be an HTTP(S) base URL without credentials or query")
	}
	nsLabel, podLabel, uidLabel := labels(t)
	podNames, uids := make([]string, 0, len(scope.Pods)), make([]string, 0, len(scope.Pods))
	for _, pod := range scope.Pods {
		if pod.Name == "" || pod.UID == "" {
			return nil, errors.New("pod name and UID are required")
		}
		podNames = append(podNames, regexp.QuoteMeta(pod.Name))
		uids = append(uids, regexp.QuoteMeta(pod.UID))
	}
	selectors := []string{nsLabel + "=" + strconv.Quote(scope.Namespace), podLabel + "=~" + strconv.Quote("("+strings.Join(podNames, "|")+")"), uidLabel + "=~" + strconv.Quote("("+strings.Join(uids, "|")+")")}
	for key, value := range t.AdditionalLabels {
		selectors = append(selectors, key+"="+strconv.Quote(value))
	}
	sort.Strings(selectors)
	seconds := int(math.Ceil(to.Sub(from).Seconds())) + 1
	if seconds < 1 || seconds > 181 {
		return nil, errors.New("unbounded native counter query")
	}
	query := metric + "{" + strings.Join(selectors, ",") + "}[" + strconv.Itoa(seconds) + "s]"
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/api/v1/query"
	args := url.Values{"query": {query}, "time": {strconv.FormatFloat(float64(to.UnixNano())/1e9, 'f', 9, 64)}}
	endpoint.RawQuery = args.Encode()
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if e != nil {
		return nil, e
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	// Redirects could leave the trusted endpoint; use a client copy with redirects disabled.
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, e := bounded.Do(req)
	if e != nil {
		return nil, e
	}
	defer response.Body.Close()
	body, e := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if e != nil {
		return nil, e
	}
	if len(body) > 2<<20 {
		return nil, errors.New("Prometheus response exceeds two MiB")
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Prometheus HTTP status %d", response.StatusCode)
	}
	var wire struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string   `json:"metric"`
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if e = json.Unmarshal(body, &wire); e != nil {
		return nil, e
	}
	if wire.Status != "success" || wire.Data.ResultType != "matrix" || len(wire.Data.Result) > 4096 {
		return nil, errors.New("invalid or oversized Prometheus matrix")
	}
	out := make([]promSeries, 0, len(wire.Data.Result))
	expected := map[PodIdentity]bool{}
	for _, pod := range scope.Pods {
		expected[pod] = true
	}
	for _, value := range wire.Data.Result {
		identity := PodIdentity{Name: value.Metric[podLabel], UID: value.Metric[uidLabel]}
		if value.Metric[nsLabel] != scope.Namespace || !expected[identity] {
			return nil, errors.New("Prometheus series violates workload identity")
		}
		if len(value.Values) > 256 {
			return nil, errors.New("too many native samples per series")
		}
		series := promSeries{Metric: value.Metric}
		for _, pair := range value.Values {
			if len(pair) != 2 {
				return nil, errors.New("invalid sample")
			}
			var timestamp float64
			var raw string
			if json.Unmarshal(pair[0], &timestamp) != nil || json.Unmarshal(pair[1], &raw) != nil {
				return nil, errors.New("invalid native sample")
			}
			number, e := strconv.ParseFloat(raw, 64)
			if e != nil || !finite(number) || !finite(timestamp) {
				return nil, errors.New("nonfinite native sample")
			}
			at := time.Unix(0, int64(math.Round(timestamp*1e9)))
			if at.After(to) {
				return nil, errors.New("future Prometheus sample")
			}
			series.Samples = append(series.Samples, promSample{At: at, Value: number})
		}
		sort.Slice(series.Samples, func(i, j int) bool { return series.Samples[i].At.Before(series.Samples[j].At) })
		out = append(out, series)
	}
	return out, nil
}
