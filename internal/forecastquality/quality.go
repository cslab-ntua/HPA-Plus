// Package forecastquality monitors raw, timestamp-matched forecast errors. It
// knows nothing about cycles, model families, replicas or scaling targets.
package forecastquality

import (
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"math"
	"sort"
	"time"
)

const MaxPending = 128

func Ensure(s *api.ForecastReliabilityState, fingerprint string) *api.ForecastReliabilityState {
	if s == nil || s.Fingerprint != fingerprint || !valid(s) {
		return &api.ForecastReliabilityState{Fingerprint: fingerprint}
	}
	return s.DeepCopy()
}

func valid(s *api.ForecastReliabilityState) bool {
	if len(s.Pending) > MaxPending || len(s.Baseline) > 100 || len(s.Recent) > 20 || s.LeadMilliseconds < 0 || math.IsNaN(s.ErrorBound) || math.IsInf(s.ErrorBound, 0) || s.ErrorBound < 0 || s.ErrorBound > 1 {
		return false
	}
	for _, values := range [][]float64{s.Baseline, s.Recent} {
		for _, v := range values {
			if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 1 {
				return false
			}
		}
	}
	for _, stamp := range []string{s.LastObservationTime, s.LastEvaluationTime, s.LastIntervalEnd} {
		if stamp != "" {
			if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
				return false
			}
		}
	}
	for _, p := range s.Pending {
		if !validPoint(p) {
			return false
		}
	}
	return true
}

func validPoint(p api.ForecastVerification) bool {
	a, e := time.Parse(time.RFC3339Nano, p.At)
	b, f := time.Parse(time.RFC3339Nano, p.Until)
	issued, g := time.Parse(time.RFC3339Nano, p.IssuedAt)
	return e == nil && f == nil && g == nil && !a.IsZero() && issued.Before(a) && b.After(a) && b.Sub(a) <= 24*time.Hour && p.CPU >= 0 && p.LeadMilliseconds > 0
}

// Record retains only a fixed-horizon, raw prediction. Duplicate target
// intervals keep the first issued forecast. Revised forecasts cannot rewrite
// evidence after the outcome is known; bounded overflow drops evidence as gaps.
func Record(s *api.ForecastReliabilityState, p *api.ForecastVerification) {
	if p == nil || !validPoint(*p) {
		return
	}
	if s.LeadMilliseconds != 0 && s.LeadMilliseconds != p.LeadMilliseconds {
		*s = api.ForecastReliabilityState{Fingerprint: s.Fingerprint}
	}
	s.LeadMilliseconds = p.LeadMilliseconds
	for _, old := range s.Pending {
		if old.At == p.At {
			return
		}
	}
	s.Pending = append(s.Pending, *p)
	sort.Slice(s.Pending, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339Nano, s.Pending[i].At)
		b, _ := time.Parse(time.RFC3339Nano, s.Pending[j].At)
		return a.Before(b)
	})
	if len(s.Pending) > MaxPending {
		s.Pending = s.Pending[len(s.Pending)-MaxPending:]
	}
}

// Observe accepts only a caller-validated complete/fresh source observation.
// It consumes at most one nonoverlapping prediction interval per source time.
// Missed intervals are discarded, never treated as zero or as successful fits.
func Observe(s *api.ForecastReliabilityState, policy api.ForecastReliabilityPolicy, at time.Time, cpu int64, now time.Time) {
	if cpu < 0 || at.IsZero() || at.After(now) {
		return
	}
	previous, _ := time.Parse(time.RFC3339Nano, s.LastObservationTime)
	if !previous.IsZero() && !at.After(previous) {
		return
	}
	s.LastObservationTime = at.UTC().Format(time.RFC3339Nano)
	lastEnd, _ := time.Parse(time.RFC3339Nano, s.LastIntervalEnd)
	pending := s.Pending[:0]
	var match *api.ForecastVerification
	for _, point := range s.Pending {
		start, _ := time.Parse(time.RFC3339Nano, point.At)
		end, _ := time.Parse(time.RFC3339Nano, point.Until)
		if !at.Before(end) {
			continue
		}
		if !at.Before(start) {
			if match == nil && !start.Before(lastEnd) {
				copy := point
				match = &copy
			}
			continue
		}
		pending = append(pending, point)
	}
	s.Pending = pending
	if len(s.Pending) == 0 {
		s.Pending = nil
	}
	if match == nil {
		return
	}
	p := policy.Effective()
	start, _ := time.Parse(time.RFC3339Nano, match.At)
	end, _ := time.Parse(time.RFC3339Nano, match.Until)
	last, _ := time.Parse(time.RFC3339Nano, s.LastEvaluationTime)
	if !last.IsZero() && at.Sub(last) > time.Duration(p.Window)*(end.Sub(start))*2 {
		s.Recent = nil
	}
	s.LastEvaluationTime = at.UTC().Format(time.RFC3339Nano)
	s.LastIntervalEnd = match.Until
	actual, predicted := float64(cpu), float64(match.CPU)
	err := (actual - predicted) / math.Max(1, math.Max(actual, predicted))
	if len(s.Baseline) < p.CalibrationSamples {
		s.Baseline = append(s.Baseline, math.Abs(err))
		s.ErrorBound = bound(s.Baseline, p.MinimumRelativeError)
		return
	}
	// The recent decision window never contributes to its own reference band.
	if len(s.Recent) == p.Window {
		if !s.Degraded {
			s.Baseline = append(s.Baseline[1:], math.Abs(s.Recent[0]))
			s.ErrorBound = bound(s.Baseline, p.MinimumRelativeError)
		}
		s.Recent = s.Recent[1:]
	}
	s.Recent = append(s.Recent, err)
	if len(s.Recent) < p.Window {
		return
	}
	under, over, inside := 0, 0, 0
	for _, e := range s.Recent {
		if e > s.ErrorBound {
			under++
		} else if e < -s.ErrorBound {
			over++
		} else {
			inside++
		}
	}
	if !s.Degraded && (under >= p.Breaches || over >= p.Breaches) {
		s.Degraded = true
		s.AdaptationPending = true
		s.Adaptation = "Requested"
	}
	if s.Degraded && !s.AdaptationPending && inside >= p.Breaches {
		s.Degraded = false
		s.Recent = nil
	}
}

func bound(values []float64, floor float64) float64 {
	v := append([]float64(nil), values...)
	sort.Float64s(v)
	return math.Max(floor, v[int(math.Ceil(.95*float64(len(v))))-1])
}

// Adapted starts verification of the next generation of forecasts. The old
// baseline stays frozen until recovery; old pending forecasts cannot score it.
func Adapted(s *api.ForecastReliabilityState, action string) {
	s.AdaptationPending = false
	s.Adaptation = action
	s.Pending = nil
	s.Recent = nil
}

func Status(s *api.ForecastReliabilityState, p api.ForecastReliabilityPolicy) *api.ForecastReliabilityStatus {
	p = p.Effective()
	r := &api.ForecastReliabilityStatus{State: "Calibrating"}
	if s == nil {
		return r
	}
	r.CalibrationSamples = len(s.Baseline)
	r.RecentSamples = len(s.Recent)
	r.ErrorBound = s.ErrorBound
	r.AdaptationPending = s.AdaptationPending
	r.Adaptation = s.Adaptation
	if len(s.Baseline) >= p.CalibrationSamples {
		r.State = "Healthy"
	}
	if s.Degraded {
		r.State = "Degraded"
	}
	if s.Adaptation == "Unsupported" {
		r.State = "Unsupported"
	}
	for _, e := range s.Recent {
		if e > s.ErrorBound {
			r.Underpredictions++
		}
		if e < -s.ErrorBound {
			r.Overpredictions++
		}
	}
	return r
}
