package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
	"io"
	"math"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"
)

// Client does not retry execution: an interrupted request may already have
// advanced a persistent model. The adapter owns its normal reset/refit fallback.
type Client struct {
	http        *http.Client
	mu          sync.Mutex
	initialized bool
	closed      bool
	initGate    chan struct{}
	calls       map[uint64]context.CancelFunc
	next        uint64
}

var errWorkerBusy = errors.New("worker service returned HTTP 503")

func NewClient(socket string) (*Client, error) {
	if !filepath.IsAbs(socket) || len(socket) > 100 {
		return nil, fmt.Errorf("worker socket must be an absolute Unix path of at most 100 bytes")
	}
	transport := &http.Transport{MaxConnsPerHost: 8, MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}
	return &Client{initGate: make(chan struct{}, 1), calls: map[uint64]context.CancelFunc{}, http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) exchange(ctx context.Context, req request) (string, error) {
	if err := req.validate(); err != nil {
		return "", err
	}
	raw, err := encode(req)
	if err != nil {
		return "", err
	}
	if len(raw) > MaxRequestBytes {
		return "", fmt.Errorf("worker request too large")
	}
	call, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://worker/v1/execute", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	call.Header.Set("Content-Type", "application/json")
	reply, err := c.http.Do(call)
	if err != nil {
		return "", err
	}
	defer reply.Body.Close()
	if reply.StatusCode == http.StatusServiceUnavailable {
		return "", errWorkerBusy
	}
	if reply.StatusCode != http.StatusOK {
		return "", fmt.Errorf("worker service returned HTTP %d", reply.StatusCode)
	}
	raw, err = io.ReadAll(io.LimitReader(reply.Body, MaxResponseBytes+1))
	if err != nil {
		return "", err
	}
	if len(raw) > MaxResponseBytes {
		return "", fmt.Errorf("worker response too large")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || (len(fields) < 3 || len(fields) > 5) || fields["value"] == nil || fields["code"] == nil || bytes.Equal(fields["value"], []byte("null")) || bytes.Equal(fields["code"], []byte("null")) {
		return "", fmt.Errorf("incomplete worker response")
	}
	var value response
	if err := decode(raw, &value); err != nil || value.Version != 1 {
		return "", fmt.Errorf("invalid worker response")
	}
	if len(value.Value) > algorithm.MaxOutputBytes || (value.Code != "" && value.Value != "") {
		return "", fmt.Errorf("invalid worker response size or state")
	}
	if req.DiagnosticsVersion == 1 && value.Code == "" && value.Diagnostics == nil {
		return "", fmt.Errorf("requested worker diagnostics missing")
	}
	if value.Diagnostics != nil {
		if req.Operation != "forecast" || value.Code != "" {
			return "", fmt.Errorf("unexpected worker diagnostics")
		}
		d, err := algorithm.DecodeForecastDiagnostics(value.Diagnostics)
		if err != nil {
			return "", fmt.Errorf("invalid worker diagnostics: %w", err)
		}
		algorithm.RecordForecastDiagnostics(ctx, d)
	}
	if value.Trajectory != nil {
		if req.Operation != "forecast" || value.Code != "" {
			return "", fmt.Errorf("unexpected worker trajectory")
		}
		points, err := algorithm.DecodeForecastTrajectory(value.Trajectory)
		if err != nil {
			return "", err
		}
		algorithm.RecordForecastTrajectory(ctx, points)
	}
	switch value.Code {
	case "":
		return value.Value, nil
	case "deadline_exceeded":
		return "", context.DeadlineExceeded
	case "canceled":
		return "", context.Canceled
	default:
		return "", errors.New("worker execution failed")
	}
}

// Reset requests rejected at admission have not run. Cancellation and admission
// release can race during shutdown, so retry only this explicit rejection, under
// the caller's cleanup deadline. Never retry execution or ambiguous failures.
func (c *Client) reset(ctx context.Context, req request) error {
	for {
		_, err := c.exchange(ctx, req)
		if !errors.Is(err, errWorkerBusy) {
			return err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func (c *Client) initialize(ctx context.Context) error {
	select {
	case c.initGate <- struct{}{}:
		defer func() { <-c.initGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	c.mu.Lock()
	closed, ready := c.closed, c.initialized
	c.mu.Unlock()
	if closed {
		return fmt.Errorf("worker client closed")
	}
	if ready {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := c.reset(ctx, request{Version: 1, Operation: "reset_all"})
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("worker client closed")
	}
	if err == nil {
		c.initialized = true
	}
	return err
}
func (c *Client) callContext(parent context.Context, timeout time.Duration) (context.Context, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, nil, fmt.Errorf("worker client closed")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	c.next++
	id := c.next
	c.calls[id] = cancel
	return ctx, func() { cancel(); c.mu.Lock(); delete(c.calls, id); c.mu.Unlock() }, nil
}
func (c *Client) Check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://worker/healthz", nil)
	if err != nil {
		return err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("worker unhealthy")
	}
	return nil
}
func (c *Client) run(ctx context.Context, op, id, script, value string, timeout int) (string, error) {
	if timeout <= 0 || int64(timeout) > math.MaxInt64/int64(time.Millisecond) {
		return "", fmt.Errorf("invalid timeout")
	}
	ctx, cancel, err := c.callContext(ctx, time.Duration(timeout)*time.Millisecond)
	if err != nil {
		return "", err
	}
	defer cancel()
	req := request{Version: 1, Operation: op, Session: id, Script: script, Payload: json.RawMessage(value), TimeoutMS: timeout}
	if op == "forecast" && algorithm.WantsForecastDiagnostics(ctx) {
		req.DiagnosticsVersion = 1
	}
	if err := req.validate(); err != nil {
		return "", err
	}
	if err := c.initialize(ctx); err != nil {
		return "", err
	}
	deadline, _ := ctx.Deadline()
	req.TimeoutMS = int(time.Until(deadline) / time.Millisecond)
	if req.TimeoutMS <= 0 {
		return "", context.DeadlineExceeded
	}
	return c.exchange(ctx, req)
}
func (c *Client) RunAlgorithmWithValue(s, v string, t int) (string, error) {
	return c.RunAlgorithmWithValueContext(context.Background(), s, v, t)
}
func (c *Client) RunAlgorithmWithValueContext(ctx context.Context, s, v string, t int) (string, error) {
	return c.run(ctx, "run", "", s, v, t)
}
func (c *Client) RunCPUForecastContext(ctx context.Context, s, v string, t int) (string, error) {
	return c.run(ctx, "forecast", "", s, v, t)
}
func (c *Client) RunSessionWithValue(id, s, v string, t int) (string, error) {
	return c.RunSessionWithValueContext(context.Background(), id, s, v, t)
}

// Bound opaque transport keys without imposing a new public model-name limit.
func sessionKey(id string) string {
	if id == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(id)))
}
func (c *Client) RunSessionWithValueContext(ctx context.Context, id, s, v string, t int) (string, error) {
	return c.run(ctx, "session", sessionKey(id), s, v, t)
}
func (c *Client) ResetSession(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.initialize(ctx); err != nil {
		return err
	}
	return c.reset(ctx, request{Version: 1, Operation: "reset", Session: sessionKey(id)})
}
func (c *Client) CloseAll() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	for _, cancel := range c.calls {
		cancel()
	}
	defer c.http.CloseIdleConnections()
	if !c.initialized {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return c.reset(ctx, request{Version: 1, Operation: "reset_all"})
}
func (c *Client) NeedLeaderElection() bool        { return true }
func (c *Client) Start(ctx context.Context) error { <-ctx.Done(); return c.CloseAll() }
