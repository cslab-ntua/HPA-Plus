package workloaddemand

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestMeasuredRequestSubintervalsDoNotSplitAveragedRate(t *testing.T) {
	from := time.Unix(1000, 0)
	pod := PodIdentity{"app", "uid"}
	scope := Scope{Namespace: "tenant-a", Pods: []PodIdentity{pod}, Lifetimes: map[string]PodLifetime{pod.UID: {Known: true, Intervals: []TimeInterval{{From: from.Add(-time.Hour)}}}}}
	points := nativePoints(from, -1, 31, 10)
	for i := range points {
		s := points[i].At.Sub(from).Seconds()
		if s > 10 {
			points[i].Value += 30 * math.Min(s-10, 10)
		}
	}
	reader := lifecycleReader(from, map[PodIdentity][]promSample{pod: points})
	cfg := testConfig()
	cfg.ForecastIntervalSeconds = 10
	got, err := reader.ReadWindow(context.Background(), scope, cfg, from, from.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ArrivalIntervals) != 3 {
		t.Fatalf("missing measured subintervals: %+v", got)
	}
	sum := 0.
	for i, p := range got.ArrivalIntervals {
		if p.Rate == nil || p.Coverage != 1 || !p.From.Equal(from.Add(time.Duration(i)*10*time.Second)) || !p.AvailableAt.Equal(got.AvailableAt) {
			t.Fatalf("invalid rate provenance: %+v", p)
		}
		sum += *p.Rate
	}
	if *got.ArrivalIntervals[1].Rate <= 2**got.ArrivalIntervals[0].Rate || math.Abs(sum/3-got.ArrivalsPerSecond) > 1e-8 {
		t.Fatal("short rates were copied from the whole-window average")
	}
}

func TestRequestSubintervalsPreserveLocalMissingSupport(t *testing.T) {
	from := time.Unix(1000, 0)
	pod := PodIdentity{"app", "uid"}
	scope := Scope{Namespace: "tenant-a", Pods: []PodIdentity{pod}, Lifetimes: map[string]PodLifetime{pod.UID: {Known: true, Intervals: []TimeInterval{{From: from.Add(-time.Hour)}}}}}
	points := nativePoints(from, -1, 31, 10)
	var retained []promSample
	for _, p := range points {
		s := p.At.Sub(from).Seconds()
		if s <= 5 || s >= 13 {
			retained = append(retained, p)
		}
	}
	reader := lifecycleReader(from, map[PodIdentity][]promSample{pod: retained})
	cfg := testConfig()
	cfg.ForecastIntervalSeconds = 10
	got, err := reader.ReadWindow(context.Background(), scope, cfg, from, from.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got.ArrivalCoverage >= .95 || len(got.ArrivalIntervals) != 3 || got.ArrivalIntervals[0].Rate != nil || got.ArrivalIntervals[1].Rate != nil || got.ArrivalIntervals[2].Rate == nil {
		t.Fatalf("missing support filled or valid short interval lost: %+v", got)
	}
}
