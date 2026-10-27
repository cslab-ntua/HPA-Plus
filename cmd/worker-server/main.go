// worker-server owns Python children in an independently limited container.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
	"github.com/cslab-ntua/HPA-Plus/internal/observability"
	"github.com/cslab-ntua/HPA-Plus/internal/worker"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	socket := flag.String("socket", "/var/run/hpaplus/worker.sock", "Private Unix execution socket")
	metrics := flag.String("metrics-bind-address", ":8082", "Read-only worker metrics address")
	memory := flag.Int("worker-memory-mib", 1024, "Per-process address-space limit")
	concurrency := flag.Int("worker-concurrency", 4, "Concurrent one-shot children")
	sessions := flag.Int("worker-sessions", 16, "Resident incremental children")
	check := flag.Bool("check", false, "Check socket health and exit")
	flag.Parse()
	client, err := worker.NewClient(*socket)
	if err != nil {
		return err
	}
	if *check {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return client.Check(ctx)
	}
	if err := algorithm.ValidateMemoryLimit(*memory); err != nil {
		return err
	}
	if *concurrency < 1 || *concurrency > 64 || *sessions < 1 || *sessions > 64 {
		return fmt.Errorf("worker limits must be 1..64")
	}
	if err := os.MkdirAll(filepath.Dir(*socket), 0770); err != nil {
		return err
	}
	// Keep the lock inode across restarts; never unlink an active server's socket.
	lock, err := os.OpenFile(*socket+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("socket already owned: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if info, err := os.Lstat(*socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refusing to replace non-socket path")
		}
		if err := os.Remove(*socket); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(*socket, 0600); err != nil {
		return err
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	telemetry, err := observability.New(registry)
	if err != nil {
		return err
	}
	one := algorithm.NewAlgorithmPython()
	one.MaxConcurrent = *concurrency
	one.MaxMemoryMiB = *memory
	one.Telemetry = telemetry
	persistent := algorithm.NewPersistentAlgorithmPython()
	persistent.MaxSessions = *sessions
	persistent.MaxMemoryMiB = *memory
	persistent.Telemetry = telemetry
	defer persistent.CloseAll()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	janitorDone := make(chan struct{})
	go func() {
		defer close(janitorDone)
		_ = algorithm.NewFeatureDiagnosticJanitor().Start(ctx)
	}()
	defer func() { cancel(); <-janitorDone }()
	service := &http.Server{Handler: worker.NewHandler(one, persistent, *concurrency+4), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192, BaseContext: func(net.Listener) context.Context { return ctx }}
	monitor := &http.Server{Addr: *metrics, Handler: monitorHandler(promhttp.HandlerFor(registry, promhttp.HandlerOpts{}), client.Check), ReadHeaderTimeout: 3 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	errors := make(chan error, 2)
	go func() { errors <- service.Serve(listener) }()
	go func() { errors <- monitor.ListenAndServe() }()
	var failure error
	select {
	case <-ctx.Done():
	case failure = <-errors:
	}
	cancel()
	shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	if err := service.Shutdown(shutdown); err != nil {
		service.Close()
		if failure == nil {
			failure = err
		}
	}
	monitor.Close()
	if failure == http.ErrServerClosed {
		return nil
	}
	return failure
}

// The network listener exposes only metrics and health. Execution stays on the
// private Unix socket. Probe the actual socket without spawning an exec process
// inside the worker's resource budget for every Kubernetes health check.
func monitorHandler(metrics http.Handler, check func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := check(ctx); err != nil {
			http.Error(w, "worker unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
