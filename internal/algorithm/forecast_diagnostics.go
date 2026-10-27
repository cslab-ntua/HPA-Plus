package algorithm

import (
	"context"
	"encoding/json"
	"fmt"
	strictjson "sigs.k8s.io/json"
	"strings"
	"sync"
	"time"
)

// ForecastDiagnostics contains bounded provenance, never training values.
// Timings are monotonic process measurements; unavailable uncertainty is explicit.
type ForecastDiagnostics struct {
	SchemaVersion        int    `json:"schemaVersion"`
	Model                string `json:"model"`
	Engine               string `json:"engine"`
	EngineVersion        string `json:"engineVersion"`
	Action               string `json:"action"`
	WindowSamples        int    `json:"windowSamples"`
	ForecastSteps        int    `json:"forecastSteps"`
	HistoryStart         string `json:"historyStart,omitempty"`
	HistoryEnd           string `json:"historyEnd,omitempty"`
	ForecastAt           string `json:"forecastAt,omitempty"`
	Uncertainty          string `json:"uncertainty"`
	TrainingNanoseconds  int64  `json:"trainingNanoseconds"`
	InferenceNanoseconds int64  `json:"inferenceNanoseconds"`
}

func (d *ForecastDiagnostics) Validate() error {
	if d == nil {
		return nil
	}
	engine := map[string]string{"Linear": "statsmodels", "HoltWinters": "statsmodels", "ARIMA": "statsmodels", "XGBoost": "xgboost", "LightGBM": "lightgbm"}[d.Model]
	if d.SchemaVersion != 1 || engine == "" || d.Engine != engine || len(d.EngineVersion) == 0 || len(d.EngineVersion) > 64 || strings.ContainsAny(d.EngineVersion, "\x00\r\n") || d.Uncertainty != "unavailable" {
		return fmt.Errorf("invalid forecast provenance")
	}
	if d.Action != "one_shot" && d.Action != "fit_forecast" && d.Action != "append_forecast" && d.Action != "forecast" {
		return fmt.Errorf("invalid forecast action")
	}
	if d.WindowSamples < 1 || d.WindowSamples > 10000 || d.ForecastSteps < 1 || d.ForecastSteps > 10000 || d.TrainingNanoseconds < 0 || d.InferenceNanoseconds < 0 || d.TrainingNanoseconds > int64(24*time.Hour) || d.InferenceNanoseconds > int64(24*time.Hour) {
		return fmt.Errorf("invalid forecast diagnostic bounds")
	}
	if d.HistoryStart == "" && d.HistoryEnd == "" && d.ForecastAt == "" {
		if d.Model == "HoltWinters" {
			return nil
		}
		return fmt.Errorf("missing forecast timestamps")
	}
	start, e1 := time.Parse(time.RFC3339Nano, d.HistoryStart)
	end, e2 := time.Parse(time.RFC3339Nano, d.HistoryEnd)
	target, e3 := time.Parse(time.RFC3339Nano, d.ForecastAt)
	if len(d.HistoryStart) > 40 || len(d.HistoryEnd) > 40 || len(d.ForecastAt) > 40 || e1 != nil || e2 != nil || e3 != nil || start.After(end) || !target.After(end) {
		return fmt.Errorf("invalid forecast diagnostic timestamps")
	}
	return nil
}

// DecodeForecastDiagnostics rejects duplicate, unknown, null and missing fields.
func DecodeForecastDiagnostics(raw json.RawMessage) (*ForecastDiagnostics, error) {
	if !json.Valid(raw) {
		return nil, fmt.Errorf("invalid diagnostic JSON")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("diagnostics must be an object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name := key.(string)
		if _, ok := fields[name]; ok {
			return nil, fmt.Errorf("duplicate diagnostic field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if string(value) == "null" {
			return nil, fmt.Errorf("null diagnostic field")
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	for _, key := range []string{"schemaVersion", "model", "engine", "engineVersion", "action", "windowSamples", "forecastSteps", "uncertainty", "trainingNanoseconds", "inferenceNanoseconds"} {
		if _, ok := fields[key]; !ok {
			return nil, fmt.Errorf("missing diagnostic field %s", key)
		}
	}
	var d ForecastDiagnostics
	issues, err := strictjson.UnmarshalStrict(raw, &d, strictjson.DisallowDuplicateFields, strictjson.DisallowUnknownFields)
	if len(issues) != 0 {
		return nil, issues[0]
	}
	if err != nil {
		return nil, err
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

type diagnosticsKey struct{}
type DiagnosticsCapture struct {
	mu   sync.Mutex
	last *ForecastDiagnostics
}

func WithForecastDiagnostics(ctx context.Context) (context.Context, *DiagnosticsCapture) {
	capture := &DiagnosticsCapture{}
	return context.WithValue(ctx, diagnosticsKey{}, capture), capture
}
func (c *DiagnosticsCapture) Last() *ForecastDiagnostics {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		return nil
	}
	copy := *c.last
	return &copy
}
func WantsForecastDiagnostics(ctx context.Context) bool {
	_, ok := ctx.Value(diagnosticsKey{}).(*DiagnosticsCapture)
	return ok
}
func RecordForecastDiagnostics(ctx context.Context, d *ForecastDiagnostics) {
	if d == nil {
		return
	}
	if c, ok := ctx.Value(diagnosticsKey{}).(*DiagnosticsCapture); ok {
		copy := *d
		c.mu.Lock()
		c.last = &copy
		c.mu.Unlock()
	}
}
