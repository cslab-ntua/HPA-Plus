// cgroup-collector reads node-local cgroup-v2 counters. It requires only a
// read-only cgroup mount and Kubernetes pod discovery, never a runtime socket.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/cslab-ntua/HPA-Plus/internal/cgroup"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func run() error {
	var node, root, listen, watchNamespace string
	var interval, retention time.Duration
	flag.StringVar(&node, "node", os.Getenv("NODE_NAME"), "node whose pod identities are accepted")
	flag.StringVar(&root, "cgroup-root", "/host-cgroup", "read-only host cgroup-v2 mount")
	flag.StringVar(&listen, "listen", ":19091", "snapshot HTTP listen address")
	flag.StringVar(&watchNamespace, "watch-namespace", "", "restrict pod discovery to this namespace; empty watches all namespaces")
	flag.DurationVar(&interval, "interval", 2*time.Second, "counter read cadence")
	flag.DurationVar(&retention, "retention", 3*time.Minute, "raw window retention")
	flag.Parse()
	if runtime.GOOS != "linux" {
		return fmt.Errorf("cgroup-v2 collection requires Linux")
	}
	collector, err := cgroup.NewCollector(root, node, interval, retention)
	if err != nil {
		return err
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	config.QPS = 2
	config.Burst = 4
	config.Timeout = 15 * time.Second
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	factory := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithNamespace(watchNamespace), informers.WithTweakListOptions(func(options *metav1.ListOptions) {
		options.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", node).String()
	}))
	pods := factory.Core().V1().Pods()
	pods.Informer()
	factory.Start(ctx.Done())
	for _, synced := range factory.WaitForCacheSync(ctx.Done()) {
		if !synced {
			return fmt.Errorf("pod discovery did not synchronize")
		}
	}
	server := &http.Server{Addr: listen, Handler: collector, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	fail := make(chan error, 1)
	go func() { fail <- server.ListenAndServe() }()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		rows, err := pods.Lister().List(labels.Everything())
		if err != nil {
			return err
		}
		collector.Collect(rows, time.Now())
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return server.Shutdown(shutdown)
		case err := <-fail:
			return err
		case <-ticker.C:
		}
	}
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
