# Live request-demand forecasting

Request-demand mode is an opt-in XGBoost path. It forecasts incoming requests per second and learns the CPU cost of completed work from normal runtime observations. It does not require a manually supplied requests-per-pod capacity. The existing CPU predictor remains the fallback when request forecasting or learned capacity is unavailable.

This first implementation supports homogeneous application pods on Linux nodes using cgroup v2. It reads existing Prometheus telemetry; it does not install an ingress, service mesh, application proxy or application instrumentation. A completed-request counter alone cannot identify arrivals that are still waiting.

## Installation and measurements

Build the current operator image, which contains `/app/cgroup-collector`, and configure Helm:

```yaml
image:
  repository: your-registry/hpa-plus
  tag: your-source-tag
cpuHistorySource: cgroup
trafficPrometheusURL: http://prometheus.monitoring.svc:9090
cgroupCollector:
  enabled: true
  # Empty watches all namespaces. Set to restrict node-side pod discovery.
  watchNamespace: application-namespace
```

The chart deploys one collector per selected Linux node. The collector has a read-only `/sys/fs/cgroup` host mount, reads counters every two seconds and retains three minutes of raw windows. It does not use a container-runtime socket or privileged mode. Pod discovery uses a node-filtered Kubernetes informer; a configured `watchNamespace` limits its RBAC to that namespace. The operator reads shared node snapshots through the Kubernetes pod proxy.

The corresponding operator flags are `--cpu-history-source=cgroup`, `--cgroup-collector-namespace`, `--cgroup-collector-selector`, `--cgroup-collector-port` and `--traffic-prometheus-url`. Helm configures the collector namespace as the release namespace. The standalone collector also accepts `--watch-namespace`.

CPU consumption is the difference between cumulative `usage_usec` counters divided by actual elapsed time. Measurements retain container IDs, pod UIDs and interval boundaries. The collector also reads memory, throttling and CPU pressure where the kernel exposes them. Missing optional counters stay unavailable. Counter resets, absent containers, missed reads and unsupported cgroup layouts leave gaps.

Both reactive CPU evaluation and CPU forecasting use the selected cgroup source with a fixed thirty-second averaging window in this initial implementation. Helm does not expose a separate averaging-window setting. Two-second collection reduces measurement delay; it does not make thirty seconds of a new container's history immediately available. A new container needs two reads for its first rate and a complete averaging window for the CPU history path. Small portions of known counter windows intersecting accounting boundaries use those windows' measured average rates; gaps are never interpolated. Changing the CPU collection contract resets incompatible stored CPU training histories.

The existing `metrics-server` default and optional `kubelet` history source remain available. Request-demand accounting requires the cgroup collector. A failed cgroup read does not silently switch to Metrics API measurements.

## Required traffic telemetry and HPAPlus configuration

Prometheus must preserve namespace, pod name and pod UID labels on the application series. The defaults are `namespace`, `pod` and `pod_uid`; `namespaceLabel`, `podLabel` and `podUIDLabel` can override their names. For example, Prometheus Kubernetes pod discovery can attach `__meta_kubernetes_pod_uid` as `pod_uid`.

Provide distinct cumulative counters for request starts and completed processing work. A cheap rejection before processing, such as an admission-control 503, must not be counted as an equally costly completed request. Use a dedicated processed-work counter, or an appropriately filtered recording rule, when the HTTP completion counter also includes rejected requests. Counters must be initialized, including zero-valued error counters, and belong to a single observation point. Document whether retries count as attempts or original requests. Do not combine both ingress and application counters for the same work. The current adapter expects counters attributed to the resolved application pods; a gateway-wide counter or an Istio completion counter cannot automatically stand in for per-pod request starts.

Use a short scrape interval, for example two seconds. The default allowed endpoint distance is three seconds (`maxAgeSeconds` can explicitly increase it up to thirty). The adapter uses native Prometheus counter timestamps, rejects counter resets and excessive gaps, and aligns CPU accounting to the corresponding completion-counter intervals. Namespace, pod names and UIDs are resolved by HPA+ and enforced on queries and returned series.

```yaml
apiVersion: hpa.plus/v1alpha1
kind: HPAPlus
metadata:
  name: application
  namespace: application-namespace
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: application
  mode: observe              # Inspect recommendations before enabling control.
  minReplicas: 1
  maxReplicas: 8
  syncPeriod: 30000          # Milliseconds; matches the initial forecasting cadence.
  includeHPA: false
  predictionFailurePolicy: HoldCurrent
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: 60
  models:
    - name: request-xgboost
      type: XGBoost
      calculationTimeout: 10000
      predictionMaxAge: 120000
      predictivePolicy:
        scaleUpLeadSeconds: 15
        tolerance: 0.1
      xgboost:
        historySize: 60
        lags: 12
        lookAhead: 90000
        forecastStrategy: direct
        seasonalFeatures: true
        nEstimators: 300
        maxDepth: 5
        objective: reg:quantileerror
        quantileAlpha: 0.95
      requestDemand:
        traffic:
          provider: prometheus
          arrivalsMetric: app_requests_started_total
          completionsMetric: app_requests_finished_total
          arrivalSemantics: started
          completionSemantics: finished
          retrySemantics: attempts
          errorsMetric: app_requests_failed_total
          latencyBucketMetric: app_request_duration_seconds_bucket
```

These metric names are illustrative: configure the actual counters exported by your telemetry source. `errorsMetric` counts failed attempts, including admission rejections, at the same observation point. The controller excludes CPU-cost accounting windows when observed errors exceed 1% of processed completions, because those windows may contain work that the successful-completion counter cannot explain. It can combine HTTP/gRPC outcomes through a correctly scoped recording rule. Latency buckets must contain seconds and an initialized `+Inf` bucket. Optional `additionalLabels` restricts a particular reporter or observation point; it cannot override the workload identity labels or histogram `le` label.

This initial mode requires an explicit `spec.syncPeriod: 30000`; omission retains the legacy fifteen-second default and is rejected for request-demand models. The request predictor uses thirty-second intervals. `lookAhead: 90000` produces three forecast intervals; other supported horizons round up to thirty-second intervals. Sixty genuine arrival samples need at least thirty minutes; missing samples extend warm-up. The configured quantile, tree count, depth, lag count and inferred seasonality are recorded in worker diagnostics. Predictors see observed telemetry and availability timestamps only. Future schedules, manually calibrated request costs and future optional-feature values are not inputs.

## Online cost estimation and decisions

For qualified accounting windows, HPA+ fits:

\[
\text{CPU-seconds}=c\times\text{completed requests}
+b\times\text{running-pod-seconds}.
\]

Here `c` estimates CPU-seconds per request and `b` estimates background CPU cores per pod. Nonnegative weighted least squares retains fifteen minutes of evidence, uses a two-minute weighting half-life and requires at least twenty qualified windows with sufficient independent variation. Constant completion rate per pod can make the two terms unidentifiable; HPA+ reports that state rather than supplying a benchmark-specific coefficient.

Qualification requires aligned and sufficiently complete counters and homogeneous pod capacity/revision. Failed-request contamination and incomplete CPU accounting exclude unsuitable windows. High utilization alone is not a reason to reject completed-work evidence. Each new observation is checked against the previous estimate before refitting: three consecutive same-direction errors above 25% mark drift, and three eligible sequential checks within that tolerance permit recovery. An estimate expires two minutes after its latest suitable observation. These are initial experimental thresholds, not accuracy guarantees.

With a qualified request forecast, the replica ratio is:

\[
N=\left\lceil\frac{\widehat{\lambda}\,c}{q\,u-b}\right\rceil,
\]

where `lambda` is incoming requests per second, `q` is CPU request per pod and `u` is the CPU target fraction. A nonpositive denominator makes this conversion unavailable. The controller then applies its existing rounding tolerance, timing, replica bounds and scaling policies. Forecasts are timestamped: higher future demand can scale up within `scaleUpLeadSeconds`; a future low forecast does not replace an already issued interval before that interval ends. Downscale stabilization defaults to zero; explicit user behavior remains effective.

The decision path is:

1. Qualified request forecast and current cost estimate: request-demand recommendation.
2. Otherwise, an eligible CPU forecast: CPU recommendation.
3. Otherwise: hold the current requested replica count.

This mode requires explicit `includeHPA: false`, `predictionFailurePolicy: HoldCurrent`, and `control` or `observe` mode. It does not use a reactive recommendation as a fallback. Reactive recommendations remain available for comparison. Existing configurations outside this mode retain their legacy failure policy. If all configured resource measurements are unavailable, the existing metric guard also prevents a scale write; inspect `MetricsAvailable` for that case.

Learning state and issued request forecasts are persisted in an owned ConfigMap, separately from CPU fallback history. Workload identity, relevant configuration and telemetry semantics fence reuse; controller restart does not make an expired estimate fresh. `status.modelStatuses[].requestDemand` reports learning state, CPU cost, background CPU, qualified sample count, last observation, expiry, selected source (`request-demand`, `cpu-forecast` or `hold`) and fallback reason.

## Optional historical inputs

The optional `requestDemand.extraMetrics` array adds up to sixteen named Prometheus features:

```yaml
requestDemand:
  traffic:
    # Same required traffic settings as above.
    provider: prometheus
    arrivalsMetric: app_requests_started_total
    completionsMetric: app_requests_finished_total
    arrivalSemantics: started
    completionSemantics: finished
    retrySemantics: attempts
  extraMetrics:
    - name: queue_depth
      metric: app_queue_depth
      unit: requests
      kind: gauge
      maxAgeSeconds: 10
    - name: active_requests
      metric: app_active_requests
      unit: requests
      kind: gauge
      maxAgeSeconds: 10
    - name: request_bytes_rate
      metric: app_request_bytes_total
      unit: bytes/second
      kind: counter
      maxAgeSeconds: 10
```

Gauges are summed across scoped pod series. Counters become rates using native counter windows; `unit` must describe the resulting rate. Every feature retains source/receipt timestamps and missingness. Missing optional data does not become zero or discard an otherwise valid arrival target. Optional features influence XGBoost inputs, not independent replica recommendations. Adding these fields does not make application queue or request-size telemetry exist; the deployment must already expose it.

Evaluate the standard-metric configuration first, then compare optional features against that frozen configuration using the same workload, model settings and policies. Request-cost learning is measured online in both cases; no saved benchmark coefficients are supplied. Fast CPU collection still measures executed CPU, and learned CPU capacity does not guarantee that application replicas solve a database, I/O, lock or downstream-service bottleneck.
