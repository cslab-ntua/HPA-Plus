package algorithm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/cslab-ntua/HPA-Plus/internal/observability"
	"io"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type readResult struct {
	value string
	err   error
}

type persistentSession struct {
	telemetry     *observability.Metrics
	algorithmPath string
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	stdout        io.ReadCloser
	reader        *bufio.Reader
	stderr        *boundedBuffer
	done          chan struct{}
	waitErr       error // published by closing done
	gate          chan struct{}
	stopOnce      sync.Once
}

func (s *persistentSession) stop() {
	s.stopOnce.Do(func() {
		killProcess(s.cmd)
		_ = s.stdin.Close()
		_ = s.stdout.Close()
		<-s.done
	})
}

func (s *persistentSession) exchange(payload string) readResult {
	if _, err := io.WriteString(s.stdin, payload+"\n"); err != nil {
		return readResult{err: err}
	}
	var response []byte
	for {
		fragment, err := s.reader.ReadSlice('\n')
		if len(response)+len(fragment) > MaxOutputBytes {
			return readResult{err: fmt.Errorf("worker response exceeds %d bytes", MaxOutputBytes)}
		}
		response = append(response, fragment...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return readResult{err: err}
		}
		return readResult{value: strings.TrimSpace(string(response))}
	}
}

func (s *persistentSession) run(ctx context.Context, payload string) (string, error) {
	queued := time.Now()
	// Cancellation while queued must not terminate the current request's worker.
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		s.telemetry.ObserveWorkerQueue("persistent", "exchange", time.Since(queued))
		return "", ctx.Err()
	}
	s.telemetry.ObserveWorkerQueue("persistent", "exchange", time.Since(queued))
	if err := ctx.Err(); err != nil {
		return "", err
	}
	select {
	case <-s.done:
		return "", fmt.Errorf("python worker exited: %s", s.stderr.String())
	default:
	}
	s.telemetry.WorkerActivity("persistent", "exchanges", 1)
	defer s.telemetry.WorkerActivity("persistent", "exchanges", -1)
	responseCh := make(chan readResult, 1)
	go func() { responseCh <- s.exchange(payload) }()
	select {
	case response := <-responseCh:
		if response.err != nil {
			s.stop()
			return "", fmt.Errorf("python worker %q: %w: %s", s.algorithmPath, response.err, s.stderr.String())
		}
		if s.stderr.overflowed() {
			s.stop()
			return "", fmt.Errorf("worker stderr exceeds %d bytes", MaxOutputBytes)
		}
		return response.value, nil
	case <-ctx.Done():
		s.stop()
		<-responseCh
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", &timeoutError{script: s.algorithmPath}
		}
		return "", ctx.Err()
	case <-s.stderr.exceeded:
		s.stop()
		<-responseCh
		return "", fmt.Errorf("worker stderr exceeds %d bytes", MaxOutputBytes)
	case <-s.done:
		s.stop()
		<-responseCh
		return "", fmt.Errorf("python worker %q exited: %v: %s", s.algorithmPath, s.waitErr, s.stderr.String())
	}
}

// PersistentPython manages bounded newline-delimited workers. Configure MaxSessions
// before first use; CloseAll permanently closes the runner.
type PersistentPython struct {
	Telemetry    *observability.Metrics
	Command      command
	Getwd        func() (string, error)
	MaxSessions  int
	MaxMemoryMiB int
	mu           sync.Mutex
	sessions     map[string]*persistentSession
	closed       bool
}

func NewPersistentAlgorithmPython() *PersistentPython {
	return &PersistentPython{Command: exec.Command, Getwd: os.Getwd, MaxSessions: 16, MaxMemoryMiB: 1024, sessions: map[string]*persistentSession{}}
}

func (p *PersistentPython) startSession(script string) (*persistentSession, error) {
	wd, err := p.Getwd()
	if err != nil {
		return nil, err
	}
	cmd, err := workerCommand(p.Command, wd, script, p.MaxMemoryMiB)
	if err != nil {
		return nil, err
	}
	prepareProcess(cmd)
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	stderr := newBoundedBuffer(MaxOutputBytes)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	s := &persistentSession{telemetry: p.Telemetry, algorithmPath: script, cmd: cmd, stdin: stdin, stdout: stdout,
		reader: bufio.NewReader(stdout), stderr: stderr, done: make(chan struct{}), gate: make(chan struct{}, 1)}
	p.Telemetry.WorkerActivity("persistent", "processes", 1)
	p.Telemetry.ObserveWorkerEvent("persistent", "started")
	go func() {
		s.waitErr = cmd.Wait()
		p.Telemetry.WorkerActivity("persistent", "processes", -1)
		p.Telemetry.ObserveWorkerEvent("persistent", "exited")
		if s.waitErr != nil {
			p.Telemetry.ObserveWorkerEvent("persistent", "failed")
		}
		close(s.done)
	}()
	go func() {
		select {
		case <-stderr.exceeded:
			s.stop()
		case <-s.done:
		}
	}()
	return s, nil
}

func (p *PersistentPython) getOrCreateSession(ctx context.Context, id, script string) (*persistentSession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Registry waits include starts and joined resets. A canceled caller must
	// not create an idle child after waiting for another process to be reaped.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.closed {
		return nil, fmt.Errorf("persistent Python runner is closed")
	}
	if p.sessions == nil {
		p.sessions = map[string]*persistentSession{}
	}
	if current := p.sessions[id]; current != nil {
		if current.algorithmPath == script {
			select {
			case <-current.done:
				delete(p.sessions, id)
			default:
				return current, nil
			}
		} else {
			current.stop()
			delete(p.sessions, id)
		}
	}
	limit := p.MaxSessions
	for key, session := range p.sessions {
		select {
		case <-session.done:
			delete(p.sessions, key)
		default:
		}
	}
	if limit <= 0 {
		limit = 16
	}
	if len(p.sessions) >= limit {
		p.Telemetry.ObserveWorkerEvent("persistent", "limit_rejected")
		return nil, fmt.Errorf("persistent Python session limit (%d) reached", limit)
	}
	// Replacing a script also joins the old process while holding this lock.
	// Cancellation during that join must not admit its replacement.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s, err := p.startSession(script)
	if err != nil {
		return nil, err
	}
	p.sessions[id] = s
	return s, nil
}

func (p *PersistentPython) RunSessionWithValue(id, script, value string, timeout int) (string, error) {
	return p.RunSessionWithValueContext(context.Background(), id, script, value, timeout)
}

func (p *PersistentPython) RunSessionWithValueContext(parent context.Context, id, script, value string, timeout int) (string, error) {
	if timeout <= 0 || int64(timeout) > math.MaxInt64/int64(time.Millisecond) {
		return "", fmt.Errorf("invalid worker timeout")
	}
	if len(value) > MaxInputBytes {
		return "", fmt.Errorf("worker input exceeds %d bytes", MaxInputBytes)
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("worker payload must be a single JSON line")
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeout)*time.Millisecond)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	queued := time.Now()
	s, err := p.getOrCreateSession(ctx, id, script)
	p.Telemetry.ObserveWorkerQueue("persistent", "session", time.Since(queued))
	if err != nil {
		return "", err
	}
	response, err := s.run(ctx, value)
	if err != nil {
		p.mu.Lock()
		// Do not evict replacements or healthy workers after queue cancellation.
		select {
		case <-s.done:
			if p.sessions[id] == s {
				delete(p.sessions, id)
			}
		default:
		}
		p.mu.Unlock()
	}
	return response, err
}

func (p *PersistentPython) ResetSession(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[id]
	if s != nil {
		p.Telemetry.ObserveWorkerEvent("persistent", "reset")
		// Keep admission occupied until Wait has reaped the old child. Otherwise
		// a concurrent replacement can temporarily exceed MaxSessions.
		s.stop()
		delete(p.sessions, id)
	}
	return nil
}

// ResetAll reaps every resident session while keeping the runner reusable.
// Used when a new manager process connects to its existing worker sidecar.
func (p *PersistentPython) ResetAll() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, session := range p.sessions {
		session.stop()
		delete(p.sessions, id)
	}
	return nil
}

func (p *PersistentPython) CloseAll() error {
	p.mu.Lock()
	p.closed = true
	sessions := p.sessions
	p.sessions = map[string]*persistentSession{}
	p.mu.Unlock()
	for _, s := range sessions {
		s.stop()
	}
	return nil
}

func (p *PersistentPython) NeedLeaderElection() bool        { return true }
func (p *PersistentPython) Start(ctx context.Context) error { <-ctx.Done(); return p.CloseAll() }
