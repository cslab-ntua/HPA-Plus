package workloaddemand

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func lifecycleReader(from time.Time, samples map[PodIdentity][]promSample) TrafficReader {
	client := mockHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queryEnd, _ := strconv.ParseFloat(r.URL.Query().Get("time"), 64)
		var result []any
		for pod, points := range samples {
			var values [][]any
			for _, p := range points {
				if float64(p.At.UnixNano())/1e9 <= queryEnd {
					values = append(values, []any{float64(p.At.UnixNano()) / 1e9, fmt.Sprint(p.Value)})
				}
			}
			result = append(result, map[string]any{"metric": map[string]string{"namespace": "tenant-a", "pod": pod.Name, "pod_uid": pod.UID}, "values": values})
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": result}})
	}))
	return TrafficReader{URL: "http://prometheus.invalid", Client: client, Now: func() time.Time { return from.Add(34 * time.Second) }}
}

func nativePoints(from time.Time, first, last, rate float64) []promSample {
	var out []promSample
	for second := first; second <= last; second += 2 {
		out = append(out, promSample{At: from.Add(time.Duration(second * 1e9)), Value: 1000 + rate*(second-first)})
	}
	return out
}

func TestArrivalBracketingAveragesPreserveNativeSourceAndNativeCostSupport(t *testing.T) {
	from := time.Unix(1000, 0)
	to := from.Add(30 * time.Second)
	pod := PodIdentity{"app", "uid"}
	scope := Scope{Namespace: "tenant-a", Pods: []PodIdentity{pod}, Lifetimes: map[string]PodLifetime{pod.UID: {Known: true, Intervals: []TimeInterval{{From: from.Add(-time.Hour)}}}}}
	r := lifecycleReader(from, map[PodIdentity][]promSample{pod: nativePoints(from, -1, 31, 10)})
	value, err := r.ReadWindow(context.Background(), scope, testConfig(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if value.ArrivalCoverage != 1 || value.ArrivalsPerSecond != 10 || !value.ArrivalBoundaryAveraged || !value.Qualified {
		t.Fatalf("bracketing lost complete measured arrival support: %+v", value)
	}
	feature := value.Features["traffic_arrivals"]
	if feature.MeasurementWindow == nil || !feature.MeasurementWindow.From.Equal(from) || !feature.MeasurementWindow.To.Equal(to) || !feature.SourceAt.Equal(to.Add(time.Second)) || !feature.ReceivedAt.Equal(to.Add(4*time.Second)) {
		t.Fatalf("native availability was relabelled: %+v", feature)
	}
	if len(value.PerPodIntervals) != 1 || !value.PerPodIntervals[0].Start.Equal(from.Add(time.Second)) || !value.PerPodIntervals[0].End.Equal(to.Add(-time.Second)) || value.CompletedRequests != 280 || math.Abs(value.NativeSupportFraction-28./30) > 1e-10 {
		t.Fatalf("cost escaped exact native support: %+v", value)
	}
}

func TestKnownAbsentRuntimeDoesNotInvalidateLivingFleet(t *testing.T) {
	from := time.Unix(1000, 0)
	to := from.Add(30 * time.Second)
	live, old := PodIdentity{"live", "live-uid"}, PodIdentity{"retired", "old-uid"}
	for _, knownAbsent := range []bool{true, false} {
		t.Run(fmt.Sprint(knownAbsent), func(t *testing.T) {
			oldLife := PodLifetime{Known: true, Intervals: []TimeInterval{{From: from.Add(-time.Minute)}}}
			if knownAbsent {
				oldLife.Intervals[0].To = from.Add(-time.Second)
			}
			scope := Scope{Namespace: "tenant-a", Pods: []PodIdentity{live, old}, Lifetimes: map[string]PodLifetime{live.UID: {Known: true, Intervals: []TimeInterval{{From: from.Add(-time.Hour)}}}, old.UID: oldLife}}
			r := lifecycleReader(from, map[PodIdentity][]promSample{live: nativePoints(from, -1, 31, 10)})
			value, err := r.ReadWindow(context.Background(), scope, testConfig(), from, to)
			if err != nil {
				t.Fatal(err)
			}
			if knownAbsent {
				if value.ArrivalCoverage != 1 || value.ArrivalsPerSecond != 10 || !value.Qualified || value.ExpectedPodSeconds != 30 {
					t.Fatalf("known absence required nonexistent counters: %+v", value)
				}
			} else if value.ArrivalCoverage != 0 || value.Qualified || value.Features["traffic_arrivals"].Value != nil {
				t.Fatalf("healthy subset claimed total fleet: %+v", value)
			}
		})
	}
}

func TestStartupGapUsesHonestCommonWallCoverageAndNeverScalesPartialPodRate(t *testing.T) {
	from := time.Unix(1000, 0)
	to := from.Add(30 * time.Second)
	live, newPod := PodIdentity{"live", "live-uid"}, PodIdentity{"new", "new-uid"}
	for _, gap := range []time.Duration{time.Second, 2 * time.Second} {
		t.Run(gap.String(), func(t *testing.T) {
			born := from.Add(10500 * time.Millisecond)
			scope := Scope{Namespace: "tenant-a", Pods: []PodIdentity{live, newPod}, Lifetimes: map[string]PodLifetime{live.UID: {Known: true, Intervals: []TimeInterval{{From: from.Add(-time.Hour)}}}, newPod.UID: {Known: true, Intervals: []TimeInterval{{From: born}}}}}
			r := lifecycleReader(from, map[PodIdentity][]promSample{live: nativePoints(from, -1, 31, 10), newPod: nativePoints(from, 10.5+gap.Seconds(), 32.5, 20)})
			value, err := r.ReadWindow(context.Background(), scope, testConfig(), from, to)
			if err != nil {
				t.Fatal(err)
			}
			want := 1 - gap.Seconds()/30
			if math.Abs(value.ArrivalCoverage-want) > 1e-10 {
				t.Fatalf("startup uncertainty hidden: got %v want %v", value.ArrivalCoverage, want)
			}
			if value.ArrivalsPerSecond >= 30 {
				t.Fatalf("partial-lifetime pod rate was applied to whole window: %+v", value)
			}
			if (value.Features["traffic_arrivals"].Value != nil) != (want >= .95) {
				t.Fatalf("95%% gate changed or missing target fabricated: %+v", value)
			}
			if len(value.ArrivalPodCoverage) != 2 || len(value.ArrivalPodCoverage[1].Gaps) == 0 {
				t.Fatal("startup support gap was not retained")
			}
		})
	}
}

func TestCounterResetAndRuntimeRestartNeverBridgeUnknownIntervals(t *testing.T) {
	from := time.Unix(1000, 0)
	to := from.Add(30 * time.Second)
	pod := PodIdentity{"app", "uid"}
	for _, runtimeRestart := range []bool{false, true} {
		scope := Scope{Namespace: "tenant-a", Pods: []PodIdentity{pod}, Lifetimes: map[string]PodLifetime{pod.UID: {Known: true, Intervals: []TimeInterval{{From: from.Add(-time.Hour)}}}}}
		points := nativePoints(from, 0, 30, 10)
		if runtimeRestart {
			life := scope.Lifetimes[pod.UID]
			life.Boundaries = []time.Time{from.Add(15 * time.Second)}
			scope.Lifetimes[pod.UID] = life
		} else {
			for i := 8; i < len(points); i++ {
				points[i].Value -= 1100
			}
		}
		r := lifecycleReader(from, map[PodIdentity][]promSample{pod: points})
		value, err := r.ReadWindow(context.Background(), scope, testConfig(), from, to)
		if err != nil {
			t.Fatal(err)
		}
		if value.ArrivalCoverage >= .95 || value.Features["traffic_arrivals"].Value != nil {
			t.Fatalf("reset/restart pair crossed as measured arrivals: %+v", value)
		}
		for _, pair := range value.PerPodNativeIntervals {
			if pair.Start.Before(from.Add(15*time.Second)) && pair.End.After(from.Add(15*time.Second)) {
				t.Fatal("cost pair crosses runtime reset")
			}
		}
	}
}

// metricReader serves a separate native series per counter name.
func metricReader(from time.Time, metrics map[string]map[PodIdentity][]promSample) TrafficReader {
	client := mockHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		queryEnd, _ := strconv.ParseFloat(r.URL.Query().Get("time"), 64)
		var result []any
		for metric, samples := range metrics {
			if !strings.HasPrefix(query, metric+"{") {
				continue
			}
			for pod, points := range samples {
				var values [][]any
				for _, p := range points {
					if float64(p.At.UnixNano())/1e9 <= queryEnd {
						values = append(values, []any{float64(p.At.UnixNano()) / 1e9, fmt.Sprint(p.Value)})
					}
				}
				result = append(result, map[string]any{"metric": map[string]string{"namespace": "tenant-a", "pod": pod.Name, "pod_uid": pod.UID}, "values": values})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": result}})
	}))
	return TrafficReader{URL: "http://prometheus.invalid", Client: client, Now: func() time.Time { return from.Add(34 * time.Second) }}
}

func TestBacklogRatioUsesTheSecondsBothCountersMeasured(t *testing.T) {
	from := time.Unix(1000, 0)
	to := from.Add(30 * time.Second)
	live, newPod := PodIdentity{"live", "live-uid"}, PodIdentity{"new", "new-uid"}
	scope := Scope{Namespace: "tenant-a", Pods: []PodIdentity{live, newPod}, Lifetimes: map[string]PodLifetime{
		live.UID: {Known: true, Intervals: []TimeInterval{{From: from.Add(-time.Hour)}}}, newPod.UID: {Known: true, Intervals: []TimeInterval{{From: from.Add(10500 * time.Millisecond)}}}}}
	read := func(liveFirst float64) TrafficWindow {
		r := metricReader(from, map[string]map[PodIdentity][]promSample{
			"app_started_total":  {live: nativePoints(from, liveFirst, 31, 10), newPod: nativePoints(from, 12.5, 32.5, 20)},
			"app_finished_total": {live: nativePoints(from, liveFirst, 31, 8), newPod: nativePoints(from, 12.5, 32.5, 16)},
		})
		window, err := r.ReadWindow(context.Background(), scope, testConfig(), from, to)
		if err != nil {
			t.Fatal(err)
		}
		return window
	}
	// The new pod's first two seconds lack counts: both rates miss the 95% gate,
	// but over the 28 seconds both counters measured, 630 arrived and 504 finished.
	window := read(-1)
	if window.Features["traffic_arrivals"].Value != nil || window.Features["traffic_completions"].Value != nil {
		t.Fatalf("partial rates were reported: %+v", window.Features)
	}
	if ratio := window.Features["traffic_backlog_ratio"].Value; ratio == nil || math.Abs(*ratio-1.25) > 1e-9 {
		t.Fatalf("matched ratio %v, want 1.25", ratio)
	}
	// With counts on fewer than half of the seconds, no ratio is reported.
	window = read(16)
	if feature := window.Features["traffic_backlog_ratio"]; feature.Value != nil || feature.MissingReason == "" {
		t.Fatalf("ratio from too little support: %+v", feature)
	}
}
