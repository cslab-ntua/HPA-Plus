package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
)

type Runner interface {
	algorithm.ContextRunner
	algorithm.CPUForecastRunner
}
type Sessions interface {
	algorithm.ContextSessionRunner
	ResetSession(string) error
	ResetAll() error
}

type Handler struct {
	runner   Runner
	sessions Sessions
	slots    chan struct{}
}

// NewHandler bounds concurrently decoded requests as well as execution in the
// underlying runners. Admission never builds an unbounded in-memory queue.
func NewHandler(r Runner, s Sessions, maxRequests int) *Handler {
	if maxRequests < 1 {
		maxRequests = 8
	}
	return &Handler{r, s, make(chan struct{}, maxRequests)}
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.URL.Path != "/v1/execute" || r.Method != http.MethodPost {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		http.Error(w, "worker busy", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
	raw, err := io.ReadAll(r.Body)
	var req request
	if err != nil || decode(raw, &req) != nil || req.validate() != nil {
		http.Error(w, "invalid worker request", http.StatusBadRequest)
		return
	}
	var value string
	forecastCtx, trajectory := algorithm.WithForecastTrajectory(r.Context())
	var diagnostics *algorithm.DiagnosticsCapture
	if req.DiagnosticsVersion == 1 {
		forecastCtx, diagnostics = algorithm.WithForecastDiagnostics(forecastCtx)
	}
	switch req.Operation {
	case "forecast":
		value, err = h.runner.RunCPUForecastContext(forecastCtx, req.Script, string(req.Payload), req.TimeoutMS)
	case "run":
		value, err = h.runner.RunAlgorithmWithValueContext(r.Context(), req.Script, string(req.Payload), req.TimeoutMS)
	case "session":
		value, err = h.sessions.RunSessionWithValueContext(r.Context(), req.Session, req.Script, string(req.Payload), req.TimeoutMS)
	case "reset":
		err = h.sessions.ResetSession(req.Session)
	case "reset_all":
		err = h.sessions.ResetAll()
	}
	reply := response{Version: 1, Value: value}
	if req.Operation == "forecast" && err == nil && len(trajectory.Last()) != 0 {
		reply.Trajectory, err = json.Marshal(trajectory.Last())
	}
	if req.Operation == "forecast" && err == nil && diagnostics != nil && diagnostics.Last() != nil {
		reply.Diagnostics, err = json.Marshal(diagnostics.Last())
	}
	if err != nil || len(value) > algorithm.MaxOutputBytes {
		reply.Value = ""
		reply.Diagnostics = nil
		reply.Trajectory = nil
		reply.Code = "execution_failed"
		if errors.Is(err, context.DeadlineExceeded) {
			reply.Code = "deadline_exceeded"
		}
		if errors.Is(err, context.Canceled) {
			reply.Code = "canceled"
		}
	}
	raw, err = encode(reply)
	if err != nil {
		http.Error(w, "encoding failed", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}
