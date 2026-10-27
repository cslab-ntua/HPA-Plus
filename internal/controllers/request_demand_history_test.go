package controllers

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	requestforecast "github.com/cslab-ntua/HPA-Plus/internal/prediction/requestdemand"
)

func TestDemandHistoryRetainsGenuineSamplesAcrossMissingSlots(t *testing.T) {
	start := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	var rows []requestforecast.Observation
	for i := 0; i < 200; i++ {
		at := start.Add(time.Duration(i) * 30 * time.Second)
		row := requestforecast.Observation{At: at, Until: at.Add(30 * time.Second), ReceivedAt: at.Add(34 * time.Second)}
		if i < 70 || i%2 == 0 {
			value := float64(i)
			row.ArrivalsPerSecond = &value
		}
		rows = retainDemandObservations(append(rows, row), 60)
		if i >= 59 {
			genuine := 0
			for j, stored := range rows {
				if stored.ArrivalsPerSecond != nil {
					genuine++
				}
				if j > 0 && stored.At.Sub(rows[j-1].At) != 30*time.Second {
					t.Fatal("missing slots were removed or timestamps changed")
				}
			}
			if genuine != 60 {
				t.Fatalf("slot %d lost genuine history: %d", i, genuine)
			}
		}
	}
	if len(rows) <= 90 || rows[len(rows)-1].ArrivalsPerSecond != nil {
		t.Fatal("did not reproduce fixed-ring starvation or missing newest sample")
	}
	raw, _ := json.Marshal(rows)
	var restarted []requestforecast.Observation
	if err := json.Unmarshal(raw, &restarted); err != nil {
		t.Fatal(err)
	}
	if len(retainDemandObservations(restarted, 60)) != len(rows) {
		t.Fatal("restart changed the retained history")
	}
}

func TestDemandHistoryBoundsLongOutagesAndLargeFeatures(t *testing.T) {
	var rows []requestforecast.Observation
	for i := 0; i < 1000; i++ {
		rows = retainDemandObservations(append(rows, requestforecast.Observation{}), 60)
	}
	if len(rows) != 240 {
		t.Fatal("outage history is unbounded")
	}
	for i := range rows {
		rows[i].Features = map[string]requestforecast.FeatureValue{}
		for k := 0; k < 32; k++ {
			rows[i].Features[fmt.Sprintf("metric-%d", k)] = feature(1, "requests", time.Now(), time.Now())
		}
	}
	bounded := retainDemandObservations(rows, 60)
	raw, _ := json.Marshal(bounded)
	if len(raw) > demandStateLimit*2/3 || len(bounded) >= len(rows) {
		t.Fatal("large feature histories exceeded their storage budget")
	}
}
