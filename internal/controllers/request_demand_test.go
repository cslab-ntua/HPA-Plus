package controllers

import (
	requestforecast "github.com/cslab-ntua/HPA-Plus/internal/prediction/requestdemand"
	"testing"
	"time"
)

func TestNewFutureForecastPreservesOnlyIssuedCurrentInterval(t *testing.T) {
	now := time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)
	old := []requestforecast.Point{{At: now, Until: now.Add(30 * time.Second), Value: 120}, {At: now.Add(30 * time.Second), Until: now.Add(time.Minute), Value: 150}}
	fresh := []requestforecast.Point{{At: now.Add(30 * time.Second), Until: now.Add(time.Minute), Value: 10}}
	merged := mergeRequestInterval(old, fresh, now.Add(5*time.Second))
	if len(merged) != 2 || merged[0].Value != 120 || merged[1].Value != 10 {
		t.Fatalf("bad interval replacement: %+v", merged)
	}
	merged = mergeRequestInterval(old, fresh, now.Add(30*time.Second))
	if merged[0].Value != 10 {
		t.Fatalf("already-expired old interval survived: %+v", merged)
	}
}

func TestReplacingLaterForecastDoesNotDiscardInterveningIssuedInterval(t *testing.T) {
	now := time.Date(2026, 10, 20, 17, 0, 4, 0, time.UTC)
	old := []requestforecast.Point{{At: now.Truncate(30 * time.Second), Until: now.Truncate(30 * time.Second).Add(30 * time.Second), Value: 100}, {At: now.Truncate(30 * time.Second).Add(30 * time.Second), Until: now.Truncate(30 * time.Second).Add(time.Minute), Value: 200}}
	fresh := []requestforecast.Point{{At: now.Truncate(30 * time.Second).Add(time.Minute), Until: now.Truncate(30 * time.Second).Add(90 * time.Second), Value: 20}}
	merged := mergeRequestInterval(old, fresh, now)
	if len(merged) != 3 || merged[1].Value != 200 {
		t.Fatalf("issued intervening forecast disappeared: %+v", merged)
	}
}
