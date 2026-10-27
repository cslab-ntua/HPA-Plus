package controllers

import (
	"encoding/json"
	"testing"
	"time"

	requestforecast "github.com/cslab-ntua/HPA-Plus/internal/prediction/requestdemand"
)

func publicationTrajectory(first time.Time) requestforecast.Result {
	result := requestforecast.Result{Status: "ready", TrainedThrough: first.Add(-30 * time.Second)}
	for i := 0; i < 3; i++ {
		at := first.Add(time.Duration(i) * 30 * time.Second)
		result.Forecasts = append(result.Forecasts, requestforecast.Point{At: at, Until: at.Add(30 * time.Second), Value: float64(20 + i)})
	}
	return result
}

func TestLateRequestPublicationRetainsOnlyFutureAndPreservesCurrent(t *testing.T) {
	boundary := time.Date(2026, 10, 20, 22, 15, 0, 0, time.UTC)
	requested, available := boundary.Add(-2*time.Second), boundary.Add(4*time.Second)
	current := requestforecast.Point{At: boundary, Until: boundary.Add(30 * time.Second), Value: 120}
	state := demandState{Forecasts: []requestforecast.Point{current}, LastForecastOutcome: "ready"}
	result := publicationTrajectory(boundary)
	publishRequestForecast(&state, result, requested, available)
	if len(state.Forecasts) != 3 || state.Forecasts[0] != current || !state.Forecasts[1].At.Equal(boundary.Add(30*time.Second)) {
		t.Fatalf("late fit erased current or future intervals: %+v", state.Forecasts)
	}
	event := state.ForecastEvents[0]
	if len(event.Forecasts) != 2 || len(event.ComputedForecasts) != 3 || event.DroppedIntervals != 1 || !event.AvailableAt.Equal(available) {
		t.Fatalf("publication evidence lacks actual availability/omitted raw point: %+v", event)
	}
	if !state.ForecastConsumedAt.Equal(result.TrainedThrough) || !state.ForecastIssuedAt.Equal(requested) || !state.ForecastAvailableAt.Equal(available) {
		t.Fatal("fit timing was backdated or source freshness extended")
	}
	// Publication may not create retroactive coverage in a cold cache.
	cold := demandState{}
	publishRequestForecast(&cold, result, requested, available)
	if len(cold.Forecasts) != 2 || !cold.Forecasts[0].At.Equal(boundary.Add(30*time.Second)) {
		t.Fatalf("retroactive bootstrap forecast: %+v", cold.Forecasts)
	}
	// A following late fit must keep the forecast already issued for its now-current interval.
	next := publicationTrajectory(boundary.Add(30 * time.Second))
	publishRequestForecast(&state, next, requested.Add(30*time.Second), available.Add(30*time.Second))
	if len(state.Forecasts) != 3 || !state.Forecasts[0].At.Equal(boundary.Add(30*time.Second)) || state.Forecasts[0].Value != 21 {
		t.Fatalf("repeated late fits left a coverage hole: %+v", state.Forecasts)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var restored demandState
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if !restored.ForecastAvailableAt.Equal(state.ForecastAvailableAt) || len(restored.ForecastEvents[0].ComputedForecasts) != 3 {
		t.Fatal("restart lost publication evidence")
	}
}

func TestEntirelyLateRequestPublicationDoesNotRefreshOldCache(t *testing.T) {
	boundary := time.Date(2026, 10, 20, 22, 15, 0, 0, time.UTC)
	old := publicationTrajectory(boundary.Add(-30 * time.Second))
	state := demandState{}
	publishRequestForecast(&state, old, boundary.Add(-35*time.Second), boundary.Add(-34*time.Second))
	issued, available, consumed := state.ForecastIssuedAt, state.ForecastAvailableAt, state.ForecastConsumedAt
	previous := append([]requestforecast.Point(nil), state.Forecasts...)
	result := publicationTrajectory(boundary)
	publishRequestForecast(&state, result, boundary.Add(-2*time.Second), boundary.Add(61*time.Second))
	if !state.ForecastIssuedAt.Equal(issued) || !state.ForecastAvailableAt.Equal(available) || !state.ForecastConsumedAt.Equal(consumed) || len(state.Forecasts) != len(previous) {
		t.Fatal("unusable new fit refreshed the old cache")
	}
	for i := range previous {
		if previous[i] != state.Forecasts[i] {
			t.Fatal("unusable fit changed old forecast")
		}
	}
	event := state.ForecastEvents[1]
	if len(event.Forecasts) != 0 || len(event.ComputedForecasts) != 3 || event.DroppedIntervals != 3 || state.LastForecastOutcome != "unavailable" || state.LastForecastReason == "" {
		t.Fatal("all-expired publication lacks failure evidence")
	}
}

func TestRequestPublicationAcrossClockPhasesAndRuntimes(t *testing.T) {
	origin := time.Date(2026, 10, 20, 22, 15, 0, 0, time.UTC)
	for phase := 0; phase < 30; phase++ {
		for _, runtime := range []time.Duration{time.Second, 6 * time.Second, 12 * time.Second, 29 * time.Second, 65 * time.Second, 95 * time.Second} {
			requested := origin.Add(time.Duration(phase) * time.Second)
			available := requested.Add(runtime)
			first := requested.Truncate(30 * time.Second).Add(30 * time.Second)
			result := publicationTrajectory(first)
			state := demandState{}
			publishRequestForecast(&state, result, requested, available)
			expected := 0
			for _, point := range result.Forecasts {
				if !point.At.Before(available) {
					expected++
				}
			}
			if len(state.Forecasts) != expected {
				t.Fatalf("phase=%d runtime=%v accepted=%d want=%d", phase, runtime, len(state.Forecasts), expected)
			}
			for _, point := range state.Forecasts {
				if point.At.Before(available) {
					t.Fatal("published already-started interval")
				}
			}
		}
	}
	// At an exact boundary the interval has not elapsed and is applicable now.
	result := publicationTrajectory(origin)
	state := demandState{}
	publishRequestForecast(&state, result, origin.Add(-time.Second), origin)
	if len(state.Forecasts) != 3 {
		t.Fatal("exact-boundary publication unnecessarily dropped current interval")
	}
	for i := 0; i < 15; i++ {
		publishRequestForecast(&state, result, origin.Add(-time.Second), origin)
	}
	if len(state.ForecastEvents) != 10 {
		t.Fatal("publication event retention became unbounded")
	}
}
