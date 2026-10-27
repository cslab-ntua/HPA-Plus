// Package worker moves Python execution into a separately limited container.
// Its execution API is served only on a private, pod-local Unix socket.
package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
	strictjson "sigs.k8s.io/json"
)

const MaxRequestBytes = algorithm.MaxInputBytes + 16*1024
const MaxResponseBytes = 6*algorithm.MaxOutputBytes + 16*1024

type request struct {
	Version            int             `json:"version"`
	Operation          string          `json:"operation"`
	Script             string          `json:"script,omitempty"`
	Session            string          `json:"session,omitempty"`
	Payload            json.RawMessage `json:"payload,omitempty"`
	TimeoutMS          int             `json:"timeoutMs,omitempty"`
	DiagnosticsVersion int             `json:"diagnosticsVersion,omitempty"`
}
type response struct {
	Trajectory  json.RawMessage `json:"trajectory,omitempty"`
	Version     int             `json:"version"`
	Value       string          `json:"value"`
	Code        string          `json:"code"`
	Diagnostics json.RawMessage `json:"diagnostics,omitempty"`
}

func decode(raw []byte, target interface{}) error {
	// Reject duplicate top-level keys as well as unknown/trailing fields.
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return fmt.Errorf("expected object")
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return fmt.Errorf("duplicate or invalid field")
		}
		seen[name] = true
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return err
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("null worker field")
		}
	}
	if _, err := d.Token(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	// encoding/json matches struct fields case-insensitively, which lets aliases
	// such as version/Version overwrite each other after the duplicate check.
	issues, err := strictjson.UnmarshalStrict(raw, target, strictjson.DisallowDuplicateFields, strictjson.DisallowUnknownFields)
	if err != nil {
		return err
	}
	if len(issues) != 0 {
		return issues[0]
	}
	return nil
}
func encode(value interface{}) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	err := enc.Encode(value)
	return b.Bytes(), err
}
func validScript(script string) bool {
	switch script {
	case "algorithms/linear_regression/linear_regression.py", "algorithms/arima/arima.py", "algorithms/holt_winters/holt_winters.py", "algorithms/xgboost/xgboost.py", "algorithms/lightgbm/lightgbm.py", "algorithms/request_demand.py":
		return true
	}
	return false
}
func (r request) validate() error {
	if (r.DiagnosticsVersion != 0 && (r.DiagnosticsVersion != 1 || r.Operation != "forecast")) || r.Version != 1 || len(r.Session) > 1024 || strings.ContainsAny(r.Session, "\x00\r\n") {
		return fmt.Errorf("invalid worker envelope")
	}
	switch r.Operation {
	case "reset_all":
		if r.Session != "" || r.Script != "" || len(r.Payload) != 0 || r.TimeoutMS != 0 {
			return fmt.Errorf("invalid reset")
		}
		return nil
	case "reset":
		if r.Session == "" || r.Script != "" || len(r.Payload) != 0 || r.TimeoutMS != 0 {
			return fmt.Errorf("invalid reset")
		}
		return nil
	case "session":
		if r.Session == "" || r.Script != "algorithms/arima/incremental_worker.py" {
			return fmt.Errorf("invalid session")
		}
	case "forecast", "run":
		if r.Session != "" || !validScript(r.Script) {
			return fmt.Errorf("unsupported script")
		}
		if r.Operation == "forecast" && r.Script == "algorithms/request_demand.py" {
			return fmt.Errorf("request demand uses its typed run protocol, not the CPU protocol")
		}
	default:
		return fmt.Errorf("unsupported operation")
	}
	if r.TimeoutMS <= 0 || int64(r.TimeoutMS) > math.MaxInt64/int64(time.Millisecond) {
		return fmt.Errorf("invalid timeout")
	}
	if len(r.Payload) > algorithm.MaxInputBytes || !bytes.HasPrefix(bytes.TrimSpace(r.Payload), []byte("{")) || !json.Valid(r.Payload) {
		return fmt.Errorf("invalid payload")
	}
	return nil
}
