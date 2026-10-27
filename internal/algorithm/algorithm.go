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
	"context"
	"errors"
	"fmt"
	"github.com/cslab-ntua/HPA-Plus/internal/observability"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	entrypoint = "python"
)

type command = func(name string, arg ...string) *exec.Cmd

func NewAlgorithmPython() *Python {
	return &Python{
		Command:      exec.Command,
		Getwd:        os.Getwd,
		MaxMemoryMiB: 1024,
	}
}

// Python is an implementation of an algorithm runner that runs algorithms using Python commands
type Python struct {
	Telemetry *observability.Metrics
	Command   command
	Getwd     func() (dir string, err error)
	// Set before first use. Zero selects four concurrent child processes.
	MaxConcurrent int
	MaxMemoryMiB  int
	once          sync.Once
	slots         chan struct{}
}

// RunAlgorithmWithValue runs an algorithm at the path provided, passing through the value provided
func (r *Python) RunAlgorithmWithValue(algorithmPath string, value string, timeout int) (string, error) {
	return r.RunAlgorithmWithValueContext(context.Background(), algorithmPath, value, timeout)
}

func (r *Python) RunAlgorithmWithValueContext(parent context.Context, algorithmPath string, value string, timeout int) (string, error) {
	if timeout <= 0 || int64(timeout) > math.MaxInt64/int64(time.Millisecond) {
		return "", fmt.Errorf("invalid algorithm timeout")
	}
	if len(value) > MaxInputBytes {
		return "", fmt.Errorf("algorithm input exceeds %d bytes", MaxInputBytes)
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeout)*time.Millisecond)
	defer cancel()
	r.once.Do(func() {
		n := r.MaxConcurrent
		if n <= 0 {
			n = 4
		}
		r.slots = make(chan struct{}, n)
	})
	queued := time.Now()
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		r.Telemetry.ObserveWorkerQueue("one_shot", "slot", time.Since(queued))
		return "", ctx.Err()
	}
	r.Telemetry.ObserveWorkerQueue("one_shot", "slot", time.Since(queued))
	if err := ctx.Err(); err != nil {
		return "", err
	}

	wd, err := r.Getwd()
	if err != nil {
		return "", err
	}

	cmd, err := workerCommand(r.Command, wd, algorithmPath, r.MaxMemoryMiB)
	if err != nil {
		return "", err
	}

	cmd.Stdin = strings.NewReader(value)
	outb, errb := newBoundedBuffer(MaxOutputBytes), newBoundedBuffer(MaxOutputBytes)
	cmd.Stdout, cmd.Stderr = outb, errb
	cmd.WaitDelay = time.Second
	prepareProcess(cmd)

	// Start command
	err = cmd.Start()
	if err != nil {
		return "", err
	}

	r.Telemetry.WorkerActivity("one_shot", "processes", 1)
	r.Telemetry.ObserveWorkerEvent("one_shot", "started")
	defer r.Telemetry.WorkerActivity("one_shot", "processes", -1)
	// Set up channel to wait for command to finish
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		r.Telemetry.ObserveWorkerEvent("one_shot", "exited")
		if err != nil {
			r.Telemetry.ObserveWorkerEvent("one_shot", "failed")
		}
		done <- err
	}()
	select {
	case <-ctx.Done():
		killProcess(cmd)
		<-done // Reap before returning; no blocked sender or racing buffer reads.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", &timeoutError{script: algorithmPath}
		}
		return "", ctx.Err()
	case <-outb.exceeded:
		killProcess(cmd)
		<-done
		return "", fmt.Errorf("algorithm stdout exceeds %d bytes", MaxOutputBytes)
	case <-errb.exceeded:
		killProcess(cmd)
		<-done
		return "", fmt.Errorf("algorithm stderr exceeds %d bytes", MaxOutputBytes)
	case err = <-done:
	}
	if outb.overflowed() || errb.overflowed() {
		return "", fmt.Errorf("algorithm output exceeds %d bytes", MaxOutputBytes)
	}
	if err != nil {
		killProcess(cmd)
		return "", fmt.Errorf("%v: %s", err, errb.String())
	}
	return outb.String(), nil
}

type timeoutError struct{ script string }

func (e *timeoutError) Error() string {
	return fmt.Sprintf("entrypoint '%s', command '%s' timed out", entrypoint, e.script)
}
func (e *timeoutError) Unwrap() error { return context.DeadlineExceeded }
