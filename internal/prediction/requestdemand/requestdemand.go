// Package requestdemand holds the closed request windows a reconstructed-CPU
// model keeps and the forecasts it publishes from them.
package requestdemand

import "time"

// MeasurementWindow is the actual interval summarized by an integrated value.
// SourceAt remains the timestamp of its backing native read, which can be later
// than To when a counter interval is averaged across an accounting boundary.
type MeasurementWindow struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

type FeatureValue struct {
	Value             *float64           `json:"value"`
	SourceAt          time.Time          `json:"sourceAt"`
	ReceivedAt        time.Time          `json:"receivedAt"`
	Unit              string             `json:"unit"`
	MeasurementWindow *MeasurementWindow `json:"measurementWindow,omitempty"`
}

// Observation is one closed thirty-second request window.
type Observation struct {
	At                      time.Time               `json:"at"`
	Until                   time.Time               `json:"until"`
	ReceivedAt              time.Time               `json:"receivedAt"`
	ArrivalsPerSecond       *float64                `json:"arrivalsPerSecond"`
	Features                map[string]FeatureValue `json:"features,omitempty"`
	ArrivalCoverage         *float64                `json:"arrivalCoverage,omitempty"`
	ArrivalBoundaryAveraged bool                    `json:"arrivalBoundaryAveraged,omitempty"`
}

type Point struct {
	At    time.Time `json:"at"`
	Until time.Time `json:"until"`
	Value float64   `json:"value"`
}

// Result is one reconstructed forecast, with values in cores.
type Result struct {
	Unit           string    `json:"unit"`
	GeneratedAt    time.Time `json:"generatedAt"`
	Status         string    `json:"status"`
	Forecasts      []Point   `json:"forecasts"`
	TrainedThrough time.Time `json:"trainedThrough,omitempty"`
}
