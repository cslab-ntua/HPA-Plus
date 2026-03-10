/*
Copyright 2022 The Predictive Horizontal Pod Autoscaler Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package algorithm

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"
)

type readResult struct {
	value string
	err   error
}

type persistentSession struct {
	algorithmPath string
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	stdout        *bufio.Reader
	stderr        *bytes.Buffer
	done          chan struct{}
	waitErr       error
	mu            sync.Mutex
}

func (s *persistentSession) run(payload string, timeout int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-s.done:
		return "", fmt.Errorf("python worker for '%s' is no longer running: %s", s.algorithmPath, strings.TrimSpace(s.stderr.String()))
	default:
	}

	_, err := io.WriteString(s.stdin, payload+"\n")
	if err != nil {
		return "", fmt.Errorf("failed to write request to python worker for '%s': %w", s.algorithmPath, err)
	}

	responseCh := make(chan readResult, 1)
	go func() {
		line, readErr := s.stdout.ReadString('\n')
		responseCh <- readResult{
			value: strings.TrimSpace(line),
			err:   readErr,
		}
	}()

	timeoutListener := time.After(time.Duration(timeout) * time.Millisecond)

	select {
	case response := <-responseCh:
		if response.err != nil {
			if response.err == io.EOF {
				return "", fmt.Errorf("python worker for '%s' exited: %s", s.algorithmPath, strings.TrimSpace(s.stderr.String()))
			}
			return "", fmt.Errorf("failed to read python worker response for '%s': %w", s.algorithmPath, response.err)
		}
		return response.value, nil
	case <-s.done:
		waitErr := s.waitErr
		if waitErr != nil {
			return "", fmt.Errorf("python worker for '%s' exited unexpectedly: %v: %s", s.algorithmPath, waitErr, strings.TrimSpace(s.stderr.String()))
		}
		return "", fmt.Errorf("python worker for '%s' exited unexpectedly", s.algorithmPath)
	case <-timeoutListener:
		return "", fmt.Errorf("entrypoint '%s', command '%s' timed out", entrypoint, s.algorithmPath)
	}
}

func (s *persistentSession) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stdin != nil {
		_ = s.stdin.Close()
	}

	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

// PersistentPython keeps long-lived Python worker processes keyed by session IDs.
type PersistentPython struct {
	Command  command
	Getwd    func() (dir string, err error)
	mu       sync.Mutex
	sessions map[string]*persistentSession
}

func NewPersistentAlgorithmPython() *PersistentPython {
	return &PersistentPython{
		Command:  exec.Command,
		Getwd:    os.Getwd,
		sessions: map[string]*persistentSession{},
	}
}

func (p *PersistentPython) startSession(algorithmPath string) (*persistentSession, error) {
	wd, err := p.Getwd()
	if err != nil {
		return nil, err
	}

	cmd := p.Command(entrypoint, path.Join(wd, algorithmPath))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	stderr := bytes.Buffer{}
	cmd.Stderr = &stderr

	err = cmd.Start()
	if err != nil {
		return nil, err
	}

	session := &persistentSession{
		algorithmPath: algorithmPath,
		cmd:           cmd,
		stdin:         stdin,
		stdout:        bufio.NewReader(stdout),
		stderr:        &stderr,
		done:          make(chan struct{}),
	}

	go func() {
		session.waitErr = cmd.Wait()
		close(session.done)
	}()

	return session, nil
}

func (p *PersistentPython) getOrCreateSession(sessionID, algorithmPath string) (*persistentSession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	current, ok := p.sessions[sessionID]
	if ok {
		if current.algorithmPath == algorithmPath {
			select {
			case <-current.done:
				delete(p.sessions, sessionID)
			default:
				return current, nil
			}
		} else {
			current.stop()
			delete(p.sessions, sessionID)
		}
	}

	session, err := p.startSession(algorithmPath)
	if err != nil {
		return nil, err
	}

	p.sessions[sessionID] = session
	return session, nil
}

// RunSessionWithValue sends a request to a long-lived Python worker session and returns its response.
func (p *PersistentPython) RunSessionWithValue(sessionID, algorithmPath, value string, timeout int) (string, error) {
	session, err := p.getOrCreateSession(sessionID, algorithmPath)
	if err != nil {
		return "", err
	}

	response, err := session.run(value, timeout)
	if err != nil {
		_ = p.ResetSession(sessionID)
		return "", err
	}
	return response, nil
}

// ResetSession terminates and removes a session.
func (p *PersistentPython) ResetSession(sessionID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	session, ok := p.sessions[sessionID]
	if !ok {
		return nil
	}

	session.stop()
	delete(p.sessions, sessionID)
	return nil
}

// CloseAll terminates and removes all active sessions.
func (p *PersistentPython) CloseAll() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for sessionID, session := range p.sessions {
		session.stop()
		delete(p.sessions, sessionID)
	}

	return nil
}
