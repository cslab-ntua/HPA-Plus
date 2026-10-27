package algorithm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	strictjson "sigs.k8s.io/json"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/uuid"
)

const CPUProtocolVersion = 1
const CPUProtocolUnit = "cpu_millicores"
const forecastDispatcher = "algorithms/forecast.py"

// CPUForecastRunner opts a runner into the versioned CPU forecast contract.
// Legacy injected runners retain their numeric interface; the production Python
// runner always uses envelopes and never retries a rejected reply as raw output.
type CPUForecastRunner interface {
	RunCPUForecastContext(context.Context, string, string, int) (string, error)
}

func RunCPUForecast(ctx context.Context, runner Runner, script, payload string, timeout int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if runner, ok := runner.(CPUForecastRunner); ok {
		return runner.RunCPUForecastContext(ctx, script, payload, timeout)
	}
	return Run(ctx, runner, script, payload, timeout)
}

type forecastRequest struct {
	ProtocolVersion    int             `json:"protocolVersion"`
	Unit               string          `json:"unit"`
	RequestID          string          `json:"requestId"`
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	DiagnosticsVersion int             `json:"diagnosticsVersion,omitempty"`
}

func forecastModel(script string) (string, error) {
	switch script {
	case "algorithms/linear_regression/linear_regression.py":
		return "Linear", nil
	case "algorithms/holt_winters/holt_winters.py":
		return "HoltWinters", nil
	case "algorithms/arima/arima.py":
		return "ARIMA", nil
	case "algorithms/xgboost/xgboost.py":
		return "XGBoost", nil
	case "algorithms/lightgbm/lightgbm.py":
		return "LightGBM", nil
	default:
		return "", fmt.Errorf("unsupported CPU forecast script")
	}
}

func (r *Python) RunCPUForecastContext(ctx context.Context, script, payload string, timeout int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(payload) > MaxInputBytes {
		return "", fmt.Errorf("forecast input exceeds %d bytes", MaxInputBytes)
	}
	model, err := forecastModel(script)
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(payload)
	if !strings.HasPrefix(trimmed, "{") || !json.Valid([]byte(payload)) {
		return "", fmt.Errorf("forecast input must be a JSON object")
	}
	request := forecastRequest{ProtocolVersion: CPUProtocolVersion, Unit: CPUProtocolUnit, RequestID: string(uuid.NewUUID()), Model: model, Input: json.RawMessage(payload)}
	if WantsForecastDiagnostics(ctx) {
		request.DiagnosticsVersion = 1
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	raw, err := r.RunAlgorithmWithValueContext(ctx, forecastDispatcher, string(encoded), timeout)
	if err != nil {
		return "", err
	}
	value, err := DecodeCPUForecastResponseContext(ctx, raw, request.RequestID)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(value, 10), nil
}

// DecodeCPUForecastResponse validates the envelope shared by one-shot and
// incremental workers. A valid zero forecast is distinct from a missing value.
func DecodeCPUForecastResponse(raw, requestID string) (int64, error) {
	return DecodeCPUForecastResponseContext(context.Background(), raw, requestID)
}

func DecodeCPUForecastResponseContext(ctx context.Context, raw, requestID string) (int64, error) {
	if len(raw) > MaxOutputBytes {
		return 0, fmt.Errorf("forecast response exceeds output limit")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return 0, fmt.Errorf("forecast response must be a JSON object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return 0, fmt.Errorf("decode forecast field: %w", err)
		}
		name, ok := key.(string)
		if !ok {
			return 0, fmt.Errorf("invalid forecast field")
		}
		if _, exists := fields[name]; exists {
			return 0, fmt.Errorf("duplicate forecast field %q", name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return 0, fmt.Errorf("decode forecast value: %w", err)
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return 0, fmt.Errorf("decode forecast object: %w", err)
	}
	if err := decoder.Decode(new(interface{})); err != io.EOF {
		return 0, fmt.Errorf("forecast response contains trailing data")
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return 0, err
	}
	var response struct {
		ProtocolVersion int             `json:"protocolVersion"`
		Unit            string          `json:"unit"`
		RequestID       string          `json:"requestId"`
		Ok              *bool           `json:"ok"`
		Prediction      *int64          `json:"prediction"`
		Error           string          `json:"error"`
		ErrorCode       string          `json:"errorCode"`
		Diagnostics     json.RawMessage `json:"diagnostics"`
		Trajectory      json.RawMessage `json:"trajectory"`
	}
	issues, err := strictjson.UnmarshalStrict(encoded, &response, strictjson.DisallowDuplicateFields, strictjson.DisallowUnknownFields)
	if len(issues) != 0 {
		return 0, fmt.Errorf("decode forecast response: %w", issues[0])
	}
	if err != nil {
		return 0, fmt.Errorf("decode forecast response: %w", err)
	}
	if response.ProtocolVersion != CPUProtocolVersion || response.Unit != CPUProtocolUnit {
		return 0, fmt.Errorf("unsupported forecast response version or unit")
	}
	if strings.TrimSpace(requestID) == "" || len(requestID) > 128 || response.RequestID != requestID {
		return 0, fmt.Errorf("forecast response does not match request")
	}
	if response.Ok == nil {
		return 0, fmt.Errorf("forecast response has no success status")
	}
	if response.ErrorCode != "" && response.ErrorCode != "invalid_input" && response.ErrorCode != "model_failed" {
		return 0, fmt.Errorf("invalid forecast error code")
	}
	if !*response.Ok {
		if response.Prediction != nil || response.Diagnostics != nil || response.Trajectory != nil {
			return 0, fmt.Errorf("forecast failure contains a result")
		}
		if response.Error == "" {
			return 0, fmt.Errorf("forecast worker returned failure without an error message")
		}
		if len([]rune(response.Error)) > 1024 {
			return 0, fmt.Errorf("forecast worker error exceeds limit")
		}
		return 0, fmt.Errorf("forecast worker failed: %s", response.Error)
	}
	if response.Prediction == nil || response.Error != "" || response.ErrorCode != "" {
		return 0, fmt.Errorf("forecast success must contain a prediction and no error")
	}
	if WantsForecastDiagnostics(ctx) && response.Diagnostics == nil {
		return 0, fmt.Errorf("requested forecast diagnostics missing")
	}
	if response.Diagnostics != nil {
		diagnostics, err := DecodeForecastDiagnostics(response.Diagnostics)
		if err != nil {
			return 0, fmt.Errorf("invalid forecast diagnostics: %w", err)
		}
		RecordForecastDiagnostics(ctx, diagnostics)
	}
	if response.Trajectory != nil {
		points, err := DecodeForecastTrajectory(response.Trajectory)
		if err != nil {
			return 0, err
		}
		RecordForecastTrajectory(ctx, points)
	}
	return *response.Prediction, nil
}
