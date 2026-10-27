// hpaplus-ui serves a local, read-only web view of the HPAPlus objects the
// current kubeconfig can read. It runs on the user's machine, not in the cluster.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/ui"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:8080", "Address to serve on; keep it on loopback")
	host := flag.String("host", "hpaplus.localtest.me", "Host name to open; *.localtest.me resolves to this machine")
	namespace := flag.String("namespace", "", "Only show this namespace (default: every namespace you can read)")
	flag.Parse()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(api.AddToScheme(scheme))
	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("no Kubernetes configuration: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	server := &ui.Server{Reader: &ui.Reader{Client: c}, Namespace: *namespace, Hosts: []string{*host, "localhost", "127.0.0.1", "::1"}}

	listeners, err := loopback(*listen)
	if err != nil {
		return err
	}
	address := *host
	if port := listeners[0].Addr().(*net.TCPAddr).Port; port != 80 {
		address += ":" + strconv.Itoa(port)
	}
	fmt.Printf("HPA+ UI on http://%s (read-only; Ctrl-C to stop)\n", address)

	httpServer := &http.Server{Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		_ = httpServer.Close()
	}()
	errs := make(chan error, len(listeners))
	for _, l := range listeners {
		go func(l net.Listener) { errs <- httpServer.Serve(l) }(l)
	}
	if err := <-errs; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// loopback listens on address. A localtest.me name resolves to both 127.0.0.1
// and ::1, so the IPv4 loopback also gets its IPv6 twin when the host has one.
func loopback(address string) ([]net.Listener, error) {
	first, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	listeners := []net.Listener{first}
	if host, _, err := net.SplitHostPort(address); err == nil && (host == "127.0.0.1" || host == "localhost") {
		port := strconv.Itoa(first.Addr().(*net.TCPAddr).Port)
		if twin, err := net.Listen("tcp", net.JoinHostPort("::1", port)); err == nil {
			listeners = append(listeners, twin)
		}
	}
	return listeners, nil
}
