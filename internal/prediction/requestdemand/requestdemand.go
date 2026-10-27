// Package requestdemand forecasts observed incoming request rates. It does not
// convert forecasts into replicas or read benchmark schedules/capacity values.
package requestdemand

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
	strictjson "sigs.k8s.io/json"
)

const Protocol = "request-demand-v1"
const Unit = "requests_per_second"
const Script = "algorithms/request_demand.py"

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

type Observation struct {
	At                      time.Time               `json:"at"`
	Until                   time.Time               `json:"until"`
	ReceivedAt              time.Time               `json:"receivedAt"`
	ArrivalsPerSecond       *float64                `json:"arrivalsPerSecond"`
	Features                map[string]FeatureValue `json:"features,omitempty"`
	ArrivalCoverage         *float64                `json:"arrivalCoverage,omitempty"`
	ArrivalBoundaryAveraged bool                    `json:"arrivalBoundaryAveraged,omitempty"`
	ArrivalIntervals        []ArrivalInterval       `json:"arrivalIntervals,omitempty"`
}

type ArrivalInterval struct {
	At               time.Time `json:"at"`
	Until            time.Time `json:"until"`
	ReceivedAt       time.Time `json:"receivedAt"`
	Rate             *float64  `json:"rate"`
	Coverage         float64   `json:"coverage"`
	BoundaryAveraged bool      `json:"boundaryAveraged"`
}

type FeatureSpec struct {
	Name          string `json:"name"`
	Unit          string `json:"unit"`
	MaxAgeSeconds int    `json:"maxAgeSeconds"`
}

// Config records the complete request-model configuration in each invocation.
// Start with Defaults or ConfigFromModel before modifying individual fields.
type Config struct {
	IntervalSeconds         int           `json:"intervalSeconds"`
	ForecastIntervalSeconds int           `json:"forecastIntervalSeconds,omitempty"`
	HistorySize             int           `json:"historySize"`
	Lags                    int           `json:"lags"`
	HorizonSteps            int           `json:"horizonSteps"`
	Quantile                float64       `json:"quantile"`
	SeasonalFeatures        bool          `json:"seasonalFeatures"`
	NEstimators             int           `json:"nEstimators"`
	MaxDepth                int           `json:"maxDepth"`
	LearningRate            float64       `json:"learningRate"`
	Subsample               float64       `json:"subsample"`
	ColsampleBytree         float64       `json:"colsampleBytree"`
	MinChildWeight          float64       `json:"minChildWeight"`
	Gamma                   float64       `json:"gamma"`
	RegLambda               float64       `json:"regLambda"`
	RegAlpha                float64       `json:"regAlpha"`
	Features                []FeatureSpec `json:"features"`
	TimeoutMillis           int           `json:"-"`
}

func Defaults() Config {
	return Config{IntervalSeconds: 30, HistorySize: 60, Lags: 12, HorizonSteps: 3,
		Quantile: .95, SeasonalFeatures: true, NEstimators: 300, MaxDepth: 5,
		LearningRate: .1, Subsample: .8, ColsampleBytree: 1, MinChildWeight: 1, Gamma: 0, RegLambda: 1, RegAlpha: 0,
		Features: []FeatureSpec{}, TimeoutMillis: 10000}
}

// ConfigFromModel keeps the CPU reference's explicit quantile, seasonality and
// tree settings. A request model always uses direct quantile regression.
func ConfigFromModel(model *hpaplusv1alpha1.Model) (Config, error) {
	c := Defaults()
	if model == nil || model.XGBoost == nil {
		return c, fmt.Errorf("request demand requires an XGBoost model")
	}
	x := model.XGBoost
	// LookAhead is expressed in milliseconds by the public XGBoost API. Keep
	// the selected horizon instead of silently replacing it with ninety seconds.
	intervalMillis := c.IntervalSeconds * 1000
	if model.RequestDemand != nil && model.RequestDemand.ForecastIntervalSeconds > 0 {
		c.ForecastIntervalSeconds = model.RequestDemand.ForecastIntervalSeconds
		switch c.ForecastIntervalSeconds {
		case 5, 10, 15, 30:
		default:
			return c, fmt.Errorf("request forecast interval must be 5, 10, 15 or 30 seconds")
		}
		intervalMillis = c.ForecastIntervalSeconds * 1000
	}
	if x.LookAhead <= 0 || x.LookAhead > 20*intervalMillis {
		return c, fmt.Errorf("request demand lookAhead must be between 1 and %d milliseconds", 20*intervalMillis)
	}
	c.HorizonSteps = 1 + (x.LookAhead-1)/intervalMillis
	if x.HistorySize > 0 {
		c.HistorySize = x.HistorySize
	}
	if x.Lags > 0 {
		c.Lags = x.Lags
	}
	c.SeasonalFeatures = x.SeasonalFeatures
	if x.QuantileAlpha != nil {
		c.Quantile = *x.QuantileAlpha
	}
	if x.NEstimators != nil {
		c.NEstimators = *x.NEstimators
	}
	if x.MaxDepth != nil {
		c.MaxDepth = *x.MaxDepth
	}
	if x.Objective != nil && *x.Objective != "reg:quantileerror" {
		return c, fmt.Errorf("request demand requires the reg:quantileerror objective")
	}
	for destination, source := range map[*float64]*float64{
		&c.LearningRate: x.LearningRate, &c.Subsample: x.Subsample,
		&c.ColsampleBytree: x.ColsampleBytree, &c.MinChildWeight: x.MinChildWeight,
		&c.Gamma: x.Gamma, &c.RegLambda: x.RegLambda, &c.RegAlpha: x.RegAlpha,
	} {
		if source != nil {
			*destination = *source
		}
	}
	if err := validateHyperparameters(c); err != nil {
		return c, err
	}
	if model.CalculationTimeout != nil {
		c.TimeoutMillis = *model.CalculationTimeout
	}
	return c, nil
}

type Point struct {
	At    time.Time `json:"at"`
	Until time.Time `json:"until"`
	Value float64   `json:"value"`
}

// FeatureAvailability counts actual finite value cells, excluding mask and
// age columns. Historical training rows use their own availability cutoffs.
type FeatureAvailability struct {
	TrainingNonMissing     int `json:"trainingNonMissing"`
	TrainingTotal          int `json:"trainingTotal"`
	InferenceNonMissing    int `json:"inferenceNonMissing"`
	InferenceTotal         int `json:"inferenceTotal"`
	TrainingRowsWithValue  int `json:"trainingRowsWithValue"`
	InferenceRowsWithValue int `json:"inferenceRowsWithValue"`
}

type Diagnostics struct {
	ForecastIntervalSeconds int                            `json:"forecastIntervalSeconds,omitempty"`
	TrainingRows            int                            `json:"trainingRows"`
	FeatureColumns          int                            `json:"featureColumns"`
	GenuineSamples          int                            `json:"genuineSamples"`
	Quantile                float64                        `json:"quantile"`
	SeasonalPeriodSeconds   *float64                       `json:"seasonalPeriodSeconds"`
	NEstimators             int                            `json:"nEstimators"`
	MaxDepth                int                            `json:"maxDepth"`
	FeatureNames            []string                       `json:"featureNames"`
	LearningRate            float64                        `json:"learningRate"`
	Subsample               float64                        `json:"subsample"`
	ColsampleBytree         float64                        `json:"colsampleBytree"`
	MinChildWeight          float64                        `json:"minChildWeight"`
	Gamma                   float64                        `json:"gamma"`
	RegLambda               float64                        `json:"regLambda"`
	RegAlpha                float64                        `json:"regAlpha"`
	Objective               string                         `json:"objective"`
	FeatureAvailability     map[string]FeatureAvailability `json:"featureAvailability,omitempty"`
	ArrivalAvailability     FeatureAvailability            `json:"arrivalAvailability"`
}

type Result struct {
	Protocol       string      `json:"protocol"`
	Unit           string      `json:"unit"`
	GeneratedAt    time.Time   `json:"generatedAt"`
	Status         string      `json:"status"`
	Reason         string      `json:"reason"`
	Forecasts      []Point     `json:"forecasts"`
	TrainedThrough time.Time   `json:"trainedThrough,omitempty"`
	Diagnostics    Diagnostics `json:"diagnostics,omitempty"`
}

type request struct {
	Protocol        string        `json:"protocol"`
	Config          Config        `json:"config"`
	Observations    []Observation `json:"observations"`
	RequestedAt     time.Time     `json:"requestedAt"`
	FirstForecastAt time.Time     `json:"firstForecastAt"`
}

// Predict uses the existing bounded worker runner. Overrides, when supplied,
// replace the model-derived configuration, making every parameter explicit.
func Predict(ctx context.Context, runner algorithm.Runner, model *hpaplusv1alpha1.Model, observations []Observation, requestedAt, firstForecastAt time.Time, overrides ...Config) (Result, error) {
	c, err := ConfigFromModel(model)
	if err != nil {
		return Result{}, err
	}
	if len(overrides) > 1 {
		return Result{}, fmt.Errorf("at most one request forecast configuration is supported")
	}
	if len(overrides) == 1 {
		c = overrides[0]
	}
	return Forecast(ctx, runner, c, observations, requestedAt, firstForecastAt)
}

// Forecast is also usable independently of the CRD model type.
func Forecast(ctx context.Context, runner algorithm.Runner, c Config, observations []Observation, requestedAt, firstForecastAt time.Time) (Result, error) {
	if runner == nil {
		return Result{}, fmt.Errorf("request forecast runner is unavailable")
	}
	if err := validate(c, observations, requestedAt, firstForecastAt); err != nil {
		return Result{}, err
	}
	c.Features = append([]FeatureSpec{}, c.Features...)
	sort.Slice(c.Features, func(i, j int) bool { return c.Features[i].Name < c.Features[j].Name })
	input, err := json.Marshal(request{Protocol, c, observations, requestedAt, firstForecastAt})
	if err != nil {
		return Result{}, fmt.Errorf("encode request forecast: %w", err)
	}
	if len(input) > algorithm.MaxInputBytes {
		return Result{}, fmt.Errorf("request forecast input exceeds worker limit")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutMillis)*time.Millisecond)
	defer cancel()
	raw, err := algorithm.Run(ctx, runner, Script, string(input), c.TimeoutMillis)
	if err != nil {
		return Result{}, err
	}
	return decode(raw, c, requestedAt, firstForecastAt)
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func validateHyperparameters(c Config) error {
	for _, parameter := range []struct {
		name  string
		value float64
	}{
		{"learningRate", c.LearningRate}, {"subsample", c.Subsample}, {"colsampleBytree", c.ColsampleBytree},
	} {
		if !finite(parameter.value) || parameter.value <= 0 || parameter.value > 1 {
			return fmt.Errorf("request forecast %s must be finite and in (0,1]", parameter.name)
		}
	}
	for _, parameter := range []struct {
		name  string
		value float64
	}{
		{"minChildWeight", c.MinChildWeight}, {"gamma", c.Gamma}, {"regLambda", c.RegLambda}, {"regAlpha", c.RegAlpha},
	} {
		if !finite(parameter.value) || parameter.value < 0 || parameter.value > 1e6 {
			return fmt.Errorf("request forecast %s must be finite and in [0,1000000]", parameter.name)
		}
	}
	return nil
}

func validate(c Config, observations []Observation, requestedAt, firstAt time.Time) error {
	frame := c.ForecastIntervalSeconds
	if frame == 0 {
		frame = c.IntervalSeconds
	}
	if frame < 1 || c.IntervalSeconds < frame || c.IntervalSeconds%frame != 0 {
		return fmt.Errorf("forecast interval must divide the history cadence")
	}
	if err := validateHyperparameters(c); err != nil {
		return err
	}
	if c.IntervalSeconds < 1 || c.IntervalSeconds > 300 || c.HistorySize < 24 || c.HistorySize > 240 || c.Lags < 1 || c.Lags > 48 || c.HorizonSteps < 1 || c.HorizonSteps > 20 || c.Lags+c.HorizonSteps+8 > c.HistorySize || !finite(c.Quantile) || c.Quantile <= 0 || c.Quantile >= 1 || c.NEstimators < 1 || c.NEstimators > 2000 || c.MaxDepth < 1 || c.MaxDepth > 16 || c.TimeoutMillis < 1 || c.TimeoutMillis > 120000 {
		return fmt.Errorf("invalid request forecast configuration")
	}
	if requestedAt.IsZero() || firstAt.Before(requestedAt) || firstAt.Nanosecond() != 0 || firstAt.Unix()%int64(frame) != 0 || firstAt.Sub(requestedAt) > 2*time.Duration(frame)*time.Second {
		return fmt.Errorf("first request forecast must begin on a future interval boundary within two intervals")
	}
	if len(observations) > 4096 || len(c.Features) > 32 {
		return fmt.Errorf("request forecast exceeds observation or feature limit")
	}
	names := map[string]bool{}
	for _, spec := range c.Features {
		if spec.Name == "" || len(spec.Name) > 128 || names[spec.Name] || spec.Unit == "" || len(spec.Unit) > 64 || spec.MaxAgeSeconds < 1 || spec.MaxAgeSeconds > 3600 {
			return fmt.Errorf("invalid request feature schema")
		}
		names[spec.Name] = true
	}
	seen := map[time.Time]bool{}
	for _, observation := range observations {
		if observation.At.IsZero() || observation.Until.Sub(observation.At) != time.Duration(c.IntervalSeconds)*time.Second || observation.At.Nanosecond() != 0 || observation.At.Unix()%int64(c.IntervalSeconds) != 0 || observation.ReceivedAt.Before(observation.Until) || seen[observation.At] {
			return fmt.Errorf("invalid or duplicate request accounting interval")
		}
		seen[observation.At] = true
		fineSeen := map[time.Time]bool{}
		if len(observation.ArrivalIntervals) > c.IntervalSeconds/frame {
			return fmt.Errorf("too many request subintervals")
		}
		for _, point := range observation.ArrivalIntervals {
			if point.At.Before(observation.At) || point.Until.After(observation.Until) || point.Until.Sub(point.At) != time.Duration(frame)*time.Second || point.At.Nanosecond() != 0 || point.At.Unix()%int64(frame) != 0 || point.ReceivedAt.Before(point.Until) || point.ReceivedAt.After(observation.ReceivedAt) || fineSeen[point.At] || !finite(point.Coverage) || point.Coverage < 0 || point.Coverage > 1 || point.Rate != nil && (!finite(*point.Rate) || *point.Rate < 0 || point.Coverage < .95) {
				return fmt.Errorf("invalid, duplicate or backdated request subinterval")
			}
			fineSeen[point.At] = true
		}
		if observation.ArrivalsPerSecond != nil && (!finite(*observation.ArrivalsPerSecond) || *observation.ArrivalsPerSecond < 0) {
			return fmt.Errorf("request arrival rate must be finite and nonnegative")
		}
		if observation.ArrivalCoverage != nil && (!finite(*observation.ArrivalCoverage) || *observation.ArrivalCoverage < 0 || *observation.ArrivalCoverage > 1 || observation.ArrivalsPerSecond != nil && *observation.ArrivalCoverage < .95) {
			return fmt.Errorf("request arrival coverage must be valid and at least 95%% for a genuine label")
		}
		for name, feature := range observation.Features {
			if !names[name] {
				continue
			}
			if feature.SourceAt.IsZero() || feature.ReceivedAt.IsZero() || feature.ReceivedAt.Before(feature.SourceAt) || (feature.Value != nil && !finite(*feature.Value)) {
				return fmt.Errorf("invalid request feature %q", name)
			}
			if window := feature.MeasurementWindow; window != nil {
				if window.From.IsZero() || window.To.IsZero() || !window.From.Before(window.To) || window.To.After(observation.Until) || feature.ReceivedAt.Before(window.To) {
					return fmt.Errorf("invalid measurement window for request feature %q", name)
				}
			} else if feature.SourceAt.After(observation.Until) {
				return fmt.Errorf("instant request feature %q comes from a future interval", name)
			}
		}
	}
	return nil
}

func decode(raw string, c Config, requestedAt, firstAt time.Time) (Result, error) {
	frame := c.ForecastIntervalSeconds
	if frame == 0 {
		frame = c.IntervalSeconds
	}
	if len(raw) > algorithm.MaxOutputBytes {
		return Result{}, fmt.Errorf("request forecast response exceeds worker limit")
	}
	var result Result
	issues, err := strictjson.UnmarshalStrict([]byte(raw), &result, strictjson.DisallowDuplicateFields, strictjson.DisallowUnknownFields)
	if err != nil || len(issues) != 0 {
		return Result{}, fmt.Errorf("invalid request forecast response")
	}
	d := json.NewDecoder(bytes.NewBufferString(raw))
	var fields map[string]json.RawMessage
	if err := d.Decode(&fields); err != nil || d.Decode(new(interface{})) != io.EOF {
		return Result{}, fmt.Errorf("invalid trailing request forecast data")
	}
	for _, key := range []string{"protocol", "unit", "generatedAt", "status", "reason", "forecasts"} {
		if len(fields[key]) == 0 || bytes.Equal(fields[key], []byte("null")) {
			return Result{}, fmt.Errorf("missing request forecast field %q", key)
		}
	}
	if result.Protocol != Protocol || result.Unit != Unit || !result.GeneratedAt.Equal(requestedAt) {
		return Result{}, fmt.Errorf("request forecast identity or unit mismatch")
	}
	if result.Status == "warming" || result.Status == "unavailable" {
		if result.Reason == "" || len(result.Forecasts) != 0 {
			return Result{}, fmt.Errorf("invalid unavailable request forecast")
		}
		return result, nil
	}
	if result.Status != "ready" || result.Reason != "" || len(result.Forecasts) != c.HorizonSteps || result.TrainedThrough.IsZero() || result.TrainedThrough.After(requestedAt) {
		return Result{}, fmt.Errorf("invalid ready request forecast")
	}
	var rawPoints []map[string]json.RawMessage
	if json.Unmarshal(fields["forecasts"], &rawPoints) != nil {
		return Result{}, fmt.Errorf("invalid request forecast points")
	}
	for i, point := range result.Forecasts {
		if len(rawPoints[i]["value"]) == 0 || bytes.Equal(rawPoints[i]["value"], []byte("null")) || !point.At.Equal(firstAt.Add(time.Duration(i*frame)*time.Second)) || point.Until.Sub(point.At) != time.Duration(frame)*time.Second || !finite(point.Value) || point.Value < 0 {
			return Result{}, fmt.Errorf("invalid request forecast interval or value")
		}
	}
	if err := validateDiagnostics(result.Diagnostics, c); err != nil {
		return Result{}, err
	}
	return result, nil
}

// Optional older diagnostics remain readable. When matrix availability is
// reported, its counts must describe the actual bounded lag matrices.
func validateDiagnostics(d Diagnostics, c Config) error {
	frame := c.ForecastIntervalSeconds
	if frame == 0 {
		frame = c.IntervalSeconds
	}
	if d.ForecastIntervalSeconds != 0 && d.ForecastIntervalSeconds != frame {
		return fmt.Errorf("request diagnostic forecast interval differs from configuration")
	}
	if d.TrainingRows < 0 || d.TrainingRows > 4096*20 {
		return fmt.Errorf("invalid request diagnostic training rows")
	}
	validate := func(v FeatureAvailability) error {
		if v.TrainingTotal != d.TrainingRows*c.Lags || v.InferenceTotal != c.HorizonSteps*c.Lags ||
			v.TrainingNonMissing < 0 || v.TrainingNonMissing > v.TrainingTotal ||
			v.InferenceNonMissing < 0 || v.InferenceNonMissing > v.InferenceTotal ||
			v.TrainingRowsWithValue < 0 || v.TrainingRowsWithValue > d.TrainingRows ||
			v.InferenceRowsWithValue < 0 || v.InferenceRowsWithValue > c.HorizonSteps ||
			v.TrainingRowsWithValue > v.TrainingNonMissing || v.InferenceRowsWithValue > v.InferenceNonMissing {
			return fmt.Errorf("invalid request diagnostic feature availability")
		}
		return nil
	}
	names := map[string]bool{}
	for _, spec := range c.Features {
		names[spec.Name] = true
	}
	for name, value := range d.FeatureAvailability {
		if !names[name] {
			return fmt.Errorf("request diagnostic reports unconfigured feature %q", name)
		}
		if err := validate(value); err != nil {
			return err
		}
	}
	if d.ArrivalAvailability != (FeatureAvailability{}) {
		return validate(d.ArrivalAvailability)
	}
	return nil
}
