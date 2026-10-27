// Package metrics adapts the legacy metrics library to request-scoped cancellation.
package metrics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jthomperoo/k8shorizmetrics/v2"
	gathered "github.com/jthomperoo/k8shorizmetrics/v2/metrics"
	"github.com/jthomperoo/k8shorizmetrics/v2/metricsclient"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/labels"
	cached "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/util/flowcontrol"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned/typed/metrics/v1beta1"
	"k8s.io/metrics/pkg/client/custom_metrics"
	"k8s.io/metrics/pkg/client/external_metrics"
)

type Gatherer interface {
	Gather(context.Context, []autoscalingv2.MetricSpec, string, labels.Selector, time.Duration, time.Duration) ([]*gathered.Metric, error)
}

// CPUHistoryGatherer collects measured CPU independently of the native HPA's
// startup-window filtering. It does not estimate missing pod/container values.
// The controller still requires a complete population and fresh observations.
type CPUHistoryGatherer interface {
	GatherCPUHistory(context.Context, autoscalingv2.MetricSpec, string, labels.Selector) (*gathered.Metric, error)
}

type IndependentCPUHistoryGatherer interface {
	CPUHistoryGatherer
	IndependentCPUHistory() bool
}

// RecentCPUGatherer measures CPU over a chosen window ending at the latest
// reads, with the CPU-history population rules. Only the cgroup source can
// choose the window.
type RecentCPUGatherer interface {
	GatherRecentCPU(context.Context, autoscalingv2.MetricSpec, string, labels.Selector, time.Duration) (*gathered.Metric, error)
}

// Kubernetes keeps configuration and the API rate budget shared, while clients
// and transports are scoped to one gather. Never retain a canceled call's clients.
type Kubernetes struct {
	config           *rest.Config
	cpuHistorySource string
	cgroup           *cgroupSource
}

func (k *Kubernetes) SetCPUHistorySource(source string) error {
	if source != "metrics-server" && source != "kubelet" && source != "cgroup" {
		return fmt.Errorf("cpu-history-source must be metrics-server, kubelet or cgroup")
	}
	k.cpuHistorySource = source
	return nil
}

func (k *Kubernetes) IndependentCPUHistory() bool {
	return k.cpuHistorySource == "kubelet" || k.cpuHistorySource == "cgroup"
}

func New(config *rest.Config) (*Kubernetes, error) {
	if config == nil {
		return nil, fmt.Errorf("missing metrics Kubernetes configuration")
	}
	copy := rest.CopyConfig(config)
	if copy.RateLimiter == nil {
		qps, burst := copy.QPS, copy.Burst
		if qps == 0 {
			qps = rest.DefaultQPS
		}
		if burst == 0 {
			burst = rest.DefaultBurst
		}
		if qps > 0 {
			if burst < 1 {
				return nil, fmt.Errorf("metrics API burst must be positive")
			}
			copy.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(qps, burst)
		}
	}
	return &Kubernetes{config: copy}, nil
}

func (k *Kubernetes) GatherCPUHistory(ctx context.Context, spec autoscalingv2.MetricSpec, namespace string, selector labels.Selector) (*gathered.Metric, error) {
	if spec.Resource == nil || spec.Resource.Name != "cpu" || spec.Resource.Target.AverageUtilization == nil {
		return nil, fmt.Errorf("CPU history requires a CPU utilization metric")
	}
	if k.cpuHistorySource == "cgroup" {
		return k.gatherCgroupCPUHistory(ctx, spec, namespace, selector)
	}
	if k.cpuHistorySource == "kubelet" {
		return k.gatherKubeletCPUHistory(ctx, spec, namespace, selector)
	}
	// A measured startup window is real CPU consumption, even when native HPA
	// deliberately excludes it from its reactive utilization calculation.
	values, err := k.Gather(ctx, []autoscalingv2.MetricSpec{spec}, namespace, selector, 0, 0)
	if err != nil {
		return nil, err
	}
	if len(values) != 1 {
		return nil, fmt.Errorf("CPU history observation unavailable")
	}
	return values[0], nil
}

func (k *Kubernetes) GatherRecentCPU(ctx context.Context, spec autoscalingv2.MetricSpec, namespace string, selector labels.Selector, window time.Duration) (*gathered.Metric, error) {
	if spec.Resource == nil || spec.Resource.Name != "cpu" || spec.Resource.Target.AverageUtilization == nil {
		return nil, fmt.Errorf("recent CPU requires a CPU utilization metric")
	}
	if k.cpuHistorySource != "cgroup" {
		return nil, fmt.Errorf("recent CPU windows need the cgroup source")
	}
	if window < 2*time.Second || window > time.Minute {
		return nil, fmt.Errorf("recent CPU window must be between 2s and 1m")
	}
	return k.gatherCgroupCPUWindow(ctx, spec, namespace, selector, window)
}

func (k *Kubernetes) Gather(parent context.Context, specs []autoscalingv2.MetricSpec, namespace string, selector labels.Selector, cpuInitialization, readinessDelay time.Duration) ([]*gathered.Metric, error) {
	// Bound all metric/discovery/pod requests and throttling together. Parent
	// cancellation (including leader shutdown) always takes precedence.
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config := rest.CopyConfig(k.config)
	config.Wrap(func(transport http.RoundTripper) http.RoundTripper {
		return &contextTransport{parent: ctx, base: transport}
	})
	if config.RateLimiter != nil {
		config.RateLimiter = &contextLimiter{RateLimiter: config.RateLimiter, parent: ctx}
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	podClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	resourceClient, err := metricsv1.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	externalClient, err := external_metrics.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	metricClient := &metricsclient.RESTClient{
		Client: resourceClient, ExternalMetricsClient: externalClient,
		CustomMetricsClient: custom_metrics.NewForConfig(config,
			restmapper.NewDeferredDiscoveryRESTMapper(cached.NewMemCacheClient(clientset.Discovery())),
			custom_metrics.NewAvailableAPIsGetter(clientset.Discovery())),
	}
	pods := &rawPodLister{client: podClient, ctx: ctx}
	var cgroup *cgroupSource
	if k.cpuHistorySource == "cgroup" {
		cgroup = k.cgroup
		if cgroup == nil {
			return nil, fmt.Errorf("cgroup CPU source not configured")
		}
	}
	gatherer := k8shorizmetrics.NewGatherer(&checkedMetricClient{RESTClient: metricClient, ctx: ctx, pods: pods, cgroup: cgroup}, pods, cpuInitialization, readinessDelay)
	var result []*gathered.Metric
	var firstError error
	for _, spec := range specs {
		// A missing/unsafe request for one resource must not suppress healthy
		// resource/object/external metrics gathered in this same observation.
		pods.resources = requestedResources([]autoscalingv2.MetricSpec{spec})
		metric, err := gatherer.GatherSingleMetricWithOptions(spec, namespace, selector, cpuInitialization, readinessDelay)
		if err != nil {
			if firstError == nil {
				firstError = err
			}
			continue
		}
		result = append(result, metric)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(result) == 0 {
		if firstError != nil {
			return nil, fmt.Errorf("no usable metric observations: %w", firstError)
		}
		return nil, fmt.Errorf("no metric observations requested")
	}
	return result, nil
}

// joinContext preserves an existing request's cancellation/deadline as well as
// the gather's context. The returned cancel stops the forwarding goroutine too.
func joinContext(parent, request context.Context) (context.Context, context.CancelFunc) {
	var ctx context.Context
	var cancel context.CancelFunc
	if deadline, ok := request.Deadline(); ok {
		ctx, cancel = context.WithDeadline(parent, deadline)
	} else {
		ctx, cancel = context.WithCancel(parent)
	}
	if request.Err() != nil {
		cancel()
		return ctx, cancel
	}
	if request.Done() != nil {
		go func() {
			select {
			case <-request.Done():
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	return ctx, cancel
}

type contextLimiter struct {
	flowcontrol.RateLimiter
	parent context.Context
}

func (l *contextLimiter) Wait(request context.Context) error {
	ctx, cancel := joinContext(l.parent, request)
	defer cancel()
	return l.RateLimiter.Wait(ctx)
}
func (l *contextLimiter) Accept() { _ = l.Wait(context.Background()) }

type contextTransport struct {
	parent context.Context
	base   http.RoundTripper
}

func (t *contextTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx, cancel := joinContext(t.parent, request.Context())
	response, err := t.base.RoundTrip(request.Clone(ctx))
	if err != nil {
		cancel()
		return response, err
	}
	// REST clients decode the body after RoundTrip returns. Keep cancellation
	// forwarding alive until ownership is released by closing the response.
	response.Body = &cancelBody{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error { defer b.cancel(); return b.ReadCloser.Close() }
