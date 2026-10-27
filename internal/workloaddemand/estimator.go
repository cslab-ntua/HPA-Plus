// Package workloaddemand provides causal online capacity estimation and scoped telemetry.
package workloaddemand

import (
	"errors"
	"fmt"
	"math"
	"time"
)

const (
	Retention      = 15 * time.Minute
	HalfLife       = 2 * time.Minute
	MaximumAge     = 2 * time.Minute
	MinimumWindows = 20
)

// Window is one observed accounting interval. CPUSeconds and CompletedRequests
// must cover the same native counter intervals; PodSeconds counts running pods.
// AvailableAt records when the complete observation first became usable.
type Window struct {
	ID                string    `json:"id"`
	Start             time.Time `json:"start"`
	End               time.Time `json:"end"`
	AvailableAt       time.Time `json:"availableAt"`
	CPUSeconds        float64   `json:"cpuSeconds"`
	CompletedRequests float64   `json:"completedRequests"`
	PodSeconds        float64   `json:"podSeconds"`
	Coverage          float64   `json:"coverage"`
	Qualified         bool      `json:"qualified"`
	Reason            string    `json:"reason,omitempty"`
}

type Estimate struct {
	CPUSecondsPerRequest float64   `json:"cpuSecondsPerRequest"`
	BackgroundCPUPerPod  float64   `json:"backgroundCPUPerPod"`
	State                string    `json:"state"`
	Reason               string    `json:"reason,omitempty"`
	QualifiedWindows     int       `json:"qualifiedWindows"`
	Version              uint64    `json:"version"`
	LastObservationAt    time.Time `json:"lastObservationAt"`
	FreshUntil           time.Time `json:"freshUntil"`
}

// State is durable and contains all information needed to resume without
// counting a window twice or resetting a drift decision after restart.
type State struct {
	Identity     string    `json:"identity"`
	Windows      []Window  `json:"windows"`
	LastEnd      time.Time `json:"lastEnd"`
	Last         Estimate  `json:"last"`
	Drifted      bool      `json:"drifted"`
	BadDirection int       `json:"badDirection"`
	Breaches     int       `json:"breaches"`
	Recovery     int       `json:"recovery"`
}

type Estimator struct{ state State }

func NewEstimator(identity string) *Estimator {
	return &Estimator{state: State{Identity: identity, Last: Estimate{State: "learning"}}}
}
func Restore(identity string, state State) (*Estimator, error) {
	if identity == "" || state.Identity != identity {
		return nil, errors.New("workload/config identity changed; estimator state cannot be reused")
	}
	e := NewEstimator(identity)
	if len(state.Windows) > 64 {
		return nil, errors.New("invalid estimator state: too many windows")
	}
	var prior time.Time
	for _, w := range state.Windows {
		if err := validWindow(w, w.AvailableAt); err != nil {
			return nil, fmt.Errorf("invalid persisted window: %w", err)
		}
		if !prior.IsZero() && !w.End.After(prior) {
			return nil, errors.New("persisted windows must be strictly ordered")
		}
		prior = w.End
	}
	if !finite(state.Last.CPUSecondsPerRequest) || !finite(state.Last.BackgroundCPUPerPod) || state.Last.CPUSecondsPerRequest < 0 || state.Last.BackgroundCPUPerPod < 0 {
		return nil, errors.New("invalid persisted estimate")
	}
	e.state = state
	e.state.Windows = append([]Window(nil), state.Windows...)
	return e, nil
}
func (e *Estimator) Snapshot() State {
	out := e.state
	out.Windows = append([]Window(nil), e.state.Windows...)
	return out
}

func validWindow(w Window, now time.Time) error {
	if w.Start.IsZero() || !w.End.After(w.Start) || w.End.Sub(w.Start) > time.Minute {
		return errors.New("accounting window must be positive and at most one minute")
	}
	if w.AvailableAt.Before(w.End) || w.AvailableAt.After(now) || w.End.After(now) {
		return errors.New("future or backdated accounting observation")
	}
	if !finite(w.CPUSeconds) || !finite(w.CompletedRequests) || !finite(w.PodSeconds) || w.CPUSeconds < 0 || w.CompletedRequests < 0 || w.PodSeconds <= 0 {
		return errors.New("invalid CPU, completion or pod accounting")
	}
	if !w.Qualified || !finite(w.Coverage) || w.Coverage < .95 || w.Coverage > 1 {
		return errors.New("accounting coverage or qualification unavailable")
	}
	return nil
}

// Observe performs validation against the preceding estimate BEFORE fitting.
// Replaying or reordering an interval never changes estimator state.
func (e *Estimator) Observe(w Window, now time.Time) Estimate {
	if !e.state.LastEnd.IsZero() && !w.End.After(e.state.LastEnd) {
		return e.Estimate(now)
	}
	if err := validWindow(w, now); err != nil {
		out := e.Estimate(now)
		out.Reason = err.Error()
		return out
	}
	// Overlapping aggregate windows would count the same work twice.
	if !e.state.LastEnd.IsZero() && w.Start.Before(e.state.LastEnd) {
		out := e.Estimate(now)
		out.Reason = "overlapping accounting window"
		return out
	}
	e.state.LastEnd = w.End
	prior := e.state.Last
	if prior.CPUSecondsPerRequest > 0 {
		predicted := prior.CPUSecondsPerRequest*w.CompletedRequests + prior.BackgroundCPUPerPod*w.PodSeconds
		// A symmetric relative error avoids an arbitrary direction-dependent threshold.
		relative := (w.CPUSeconds - predicted) / math.Max(math.Max(w.CPUSeconds, predicted), 1e-9)
		if math.Abs(relative) > .25 {
			direction := 1
			if relative < 0 {
				direction = -1
			}
			if direction == e.state.BadDirection {
				e.state.Breaches++
			} else {
				e.state.BadDirection = direction
				e.state.Breaches = 1
			}
			e.state.Recovery = 0
			if e.state.Breaches >= 3 {
				e.state.Drifted = true
			}
		} else {
			e.state.BadDirection = 0
			e.state.Breaches = 0
			if e.state.Drifted {
				e.state.Recovery++
				if e.state.Recovery >= 3 {
					e.state.Drifted = false
					e.state.Recovery = 0
				}
			}
		}
	}
	cutoff := w.End.Add(-Retention)
	kept := e.state.Windows[:0]
	for _, old := range e.state.Windows {
		if !old.End.Before(cutoff) {
			kept = append(kept, old)
		}
	}
	e.state.Windows = append(kept, w)
	estimate := Estimate{State: "learning", Reason: "requires at least twenty qualified windows", QualifiedWindows: len(e.state.Windows), Version: prior.Version, LastObservationAt: w.End, FreshUntil: w.End.Add(MaximumAge)}
	if len(e.state.Windows) >= MinimumWindows {
		c, b, ok := fit(e.state.Windows, w.End)
		if !ok {
			estimate.State = "unidentifiable"
			estimate.Reason = "request completions and pod time lack independent variation"
		} else {
			estimate.CPUSecondsPerRequest = c
			estimate.BackgroundCPUPerPod = b
			estimate.Version++
			estimate.State = "qualified"
			estimate.Reason = ""
		}
	}
	if e.state.Drifted {
		estimate.State = "drifted"
		estimate.Reason = "three consecutive same-direction accounting errors above 25 percent"
	}
	e.state.Last = estimate
	return e.Estimate(now)
}

func (e *Estimator) Estimate(now time.Time) Estimate {
	out := e.state.Last
	if !out.FreshUntil.IsZero() && !now.Before(out.FreshUntil) {
		out.State = "stale"
		out.Reason = "no suitable accounting evidence for two minutes"
	}
	return out
}

// RequiredReplicas returns the unrounded demand ratio; the controller applies
// its existing tolerance, timing, replica bounds and write policies.
func RequiredReplicas(arrivalsPerSecond, requestCores, targetUtilization float64, estimate Estimate, now time.Time) (float64, error) {
	if estimate.State != "qualified" || !now.Before(estimate.FreshUntil) {
		return 0, errors.New("CPU-cost estimate is unavailable")
	}
	if !finite(arrivalsPerSecond) || arrivalsPerSecond < 0 || !finite(requestCores) || requestCores <= 0 || !finite(targetUtilization) || targetUtilization <= 0 || targetUtilization > 1 {
		return 0, errors.New("invalid arrival forecast or pod CPU capacity")
	}
	denominator := requestCores*targetUtilization - estimate.BackgroundCPUPerPod
	if denominator <= 0 {
		return 0, errors.New("background CPU consumes the target capacity per pod")
	}
	value := arrivalsPerSecond * estimate.CPUSecondsPerRequest / denominator
	if !finite(value) {
		return 0, errors.New("nonfinite replica demand")
	}
	return value, nil
}

func fit(windows []Window, now time.Time) (float64, float64, bool) {
	var xx, xz, zz, xy, zy float64
	for _, w := range windows {
		weight := math.Exp2(-now.Sub(w.End).Seconds() / HalfLife.Seconds())
		x, z, y := w.CompletedRequests, w.PodSeconds, w.CPUSeconds
		xx += weight * x * x
		xz += weight * x * z
		zz += weight * z * z
		xy += weight * x * y
		zy += weight * z * y
	}
	determinant := xx*zz - xz*xz
	if xx <= 0 || zz <= 0 || determinant/(xx*zz) < 1e-4 {
		return 0, 0, false
	}
	c, b := (xy*zz-zy*xz)/determinant, (zy*xx-xy*xz)/determinant
	candidates := [][2]float64{{math.Max(0, xy/xx), 0}, {0, math.Max(0, zy/zz)}}
	if c >= 0 && b >= 0 {
		candidates = append(candidates, [2]float64{c, b})
	}
	best := math.Inf(1)
	var result [2]float64
	for _, candidate := range candidates {
		loss := candidate[0]*candidate[0]*xx + 2*candidate[0]*candidate[1]*xz + candidate[1]*candidate[1]*zz - 2*candidate[0]*xy - 2*candidate[1]*zy
		if loss < best {
			best = loss
			result = candidate
		}
	}
	if !finite(result[0]) || !finite(result[1]) || result[0] <= 0 {
		return 0, 0, false
	}
	return result[0], result[1], true
}
func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }
