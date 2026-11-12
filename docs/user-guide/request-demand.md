# Request demand

CPU history cannot show demand that saturated pods failed to serve: a pod at its CPU limit uses the same CPU whether
one request or a hundred are waiting. Every HPA+ model forecasts request demand: HPA+ rebuilds that demand from
request counters. Each closed thirty-second window is rebuilt as measured CPU multiplied by the
requests that arrived per request completed. While pods keep up the ratio is about one; when completions fall behind
it rises, so a saturated pod no longer hides the work it could not serve.

The model then forecasts this rebuilt CPU with its own forecaster. Any model type can use it: Linear, Holt-Winters,
ARIMA, XGBoost, LightGBM, Toto, TiRex and seasonal naive. No requests-per-pod capacity or per-request CPU cost is supplied or learned.

Request demand supports homogeneous application pods on Linux nodes with cgroup v2. It reads Prometheus telemetry; it
does not install an ingress, service mesh, application proxy or application instrumentation.

## What it needs today

HPA+ currently reads:

- per-pod counters of requests started and requests finished, in Prometheus, with namespace, pod and pod UID labels;
- those counters scraped about every 2 seconds;
- CPU from the cgroup collector, on Linux nodes with cgroup v2;
- `spec.syncPeriod: 30000`.

These come from our test setup, where the application exports both counters. They are not the only way to get the
numbers. If your application gives requests started and finished another way, open an issue or a pull request and we
can adjust HPA+ to read it. Two examples:

- an in-flight gauge: requests started = requests finished + the change in requests in flight;
- a latency histogram: requests in the system ≈ requests finished per second × mean latency (Little's law), so
  requests started ≈ requests finished + the growth of that number.

Without these counters HPA+ has nothing to forecast, and an HPAPlus without models gives only the HPA formula.
An earlier version could forecast measured CPU alone. It was removed because it did much worse: when pods are full
their CPU stops at what they can do.

## Installation and measurements

The operator image contains the collector, `/app/cgroup-collector`, and the chart enables it by default. Point HPA+ at
the Prometheus that scrapes your request counters:

```yaml
image:
  repository: your-registry/hpa-plus
  tag: your-source-tag
trafficPrometheusURL: http://prometheus.monitoring.svc:9090
cgroupCollector:
  # Empty watches all namespaces. Set to restrict node-side pod discovery.
  watchNamespace: application-namespace
```

The collector reads `/sys/fs/cgroup` through a read-only hostPath mount. Clusters that enforce the baseline Pod
Security level must allow it in the operator's namespace.

The chart deploys one collector per selected Linux node. The collector has a read-only `/sys/fs/cgroup` host mount, reads counters every two seconds and retains three minutes of raw windows. It does not use a container-runtime socket or privileged mode. Pod discovery uses a node-filtered Kubernetes informer; a configured `watchNamespace` limits its RBAC to that namespace. The operator reads shared node snapshots through the Kubernetes pod proxy.

The corresponding operator flags are `--cpu-history-source=cgroup`, `--cgroup-collector-namespace`, `--cgroup-collector-selector`, `--cgroup-collector-port` and `--traffic-prometheus-url`. Helm configures the collector namespace as the release namespace. The standalone collector also accepts `--watch-namespace`.

CPU consumption is the difference between cumulative `usage_usec` counters divided by actual elapsed time. Measurements retain container IDs, pod UIDs and interval boundaries. The collector also reads memory, throttling and CPU pressure where the kernel exposes them. Missing optional counters stay unavailable. Counter resets, absent containers, missed reads and unsupported cgroup layouts leave gaps.

Both reactive CPU evaluation and CPU forecasting use the selected cgroup source with a fixed thirty-second averaging window. Helm does not expose a separate averaging-window setting. Two-second collection reduces measurement delay; it does not make thirty seconds of a new container's history immediately available. A new container needs two reads for its first rate and a complete averaging window for the CPU history path. Gaps are never interpolated. Changing the CPU collection contract resets incompatible stored CPU training histories.

Request demand requires the cgroup collector. The `metrics-server` and `kubelet` sources in `cpuHistorySource` only suit an HPAPlus without models. A failed cgroup read does not silently switch to Metrics API measurements.

## Required traffic telemetry and HPAPlus configuration

Prometheus must preserve namespace, pod name and pod UID labels on the application series. The defaults are `namespace`, `pod` and `pod_uid`; `namespaceLabel`, `podLabel` and `podUIDLabel` can override their names. For example, Prometheus Kubernetes pod discovery can attach `__meta_kubernetes_pod_uid` as `pod_uid`.

Provide distinct cumulative counters for request starts and completed processing work, counted per application pod at a single observation point. A cheap rejection before processing, such as an admission-control 503, must not be counted as completed work; use a dedicated processed-work counter, or an appropriately filtered recording rule, when the HTTP completion counter also includes rejected requests. Counters must be initialized. Document whether retries count as attempts or original requests. Do not combine both ingress and application counters for the same work. A gateway-wide counter or an Istio completion counter cannot automatically stand in for per-pod request starts.

Use a short scrape interval, for example two seconds. The default allowed endpoint distance is three seconds (`maxAgeSeconds` can explicitly increase it up to thirty). HPA+ uses native Prometheus counter timestamps, rejects counter resets and excessive gaps, and divides arrivals by completions only over the seconds both counters measured on every running pod. Namespace, pod names and UIDs are resolved by HPA+ and enforced on queries and returned series.

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
  syncPeriod: 30000          # Milliseconds; request demand needs 30000.
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
    - name: demand
      type: Linear           # Any model type works.
      calculationTimeout: 10000
      predictionMaxAge: 120000
      predictivePolicy:
        scaleUpLeadSeconds: 15
        tolerance: 0.1
      linear:
        historySize: 20
        lookAhead: 90000
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

These metric names are illustrative: configure the actual counters exported by your telemetry source. `errorsMetric`, `outcomesMetric` and `latencyBucketMetric` are optional. They do not change the scaling decision; the [local UI](ui.md) shows them as failed requests and response time. `errorsMetric` counts failed attempts at the same observation point, and the optional `outcomesMetric` counts every finished HTTP/gRPC outcome, errors included, so that the error fraction can be shown. Latency buckets must contain seconds and an initialized `+Inf` bucket. Optional `additionalLabels` restricts a particular reporter or observation point; it cannot override the workload identity labels or histogram `le` label.

Request demand requires an explicit `spec.syncPeriod: 30000`; leaving it out means the fifteen-second default, which is rejected. It also requires `predictivePolicy`, `predictionMaxAge`, `includeHPA: false`, `predictionFailurePolicy: HoldCurrent`, and `control` or `observe` mode. `forecastTarget` is optional; its only value is `reconstructedCPU`.

The model forecasts once per closed thirty-second window, after that window's CPU sample exists. It needs as many rebuilt windows as its own history size: `historySize` for Linear, ARIMA, XGBoost, LightGBM, Toto, TiRex and seasonal naive, or `seasonalPeriods` times `storedSeasons` for Holt-Winters. A window without request counts keeps its measured CPU, and `reconstructionFallbacks` counts those windows.

## Decisions

The recommendation never falls below the replicas that the current reconstructed demand needs. That floor is measured at decision time over the most recent half sampling period, so a sudden step is acted on within seconds. A shorter fast window, a third of the floor window, can only add capacity, and decisions run at least once per fast window. A forecast can still add capacity early, before a predicted rise. While demand is still rising, HPA+ does not remove pods. When a newer forecast takes back a rise that the previous one expected by now, the previous forecast's pods are kept until the window in progress ends.

The decision path is:

1. A usable reconstructed forecast: the request-demand recommendation, with the floor.
2. Otherwise, the model's ordinary CPU forecast, for example during warm-up or when the request counters are unavailable.
3. Otherwise: hold the current requested replica count.

It never uses a reactive recommendation as a fallback; reactive recommendations remain available for comparison. If all configured resource measurements are unavailable, the metric guard also prevents a scale write; inspect `MetricsAvailable` for that case.

The request windows and issued forecasts are persisted in an owned ConfigMap, separately from the CPU history. Workload identity, relevant configuration and the Prometheus origin fence reuse; a change starts a new history, and a controller restart does not make an expired forecast fresh.

`status.modelStatuses[].requestDemand` reports:

- `state`, `source` (`request-demand`, `cpu-forecast` or `hold`) and `fallbackReason`;
- `measuredCores` and `reconstructedCores` of the latest window, and `reconstructionFallbacks`;
- `forecastOnlyReplicas`, `floorReplicas` and `decidedBy`: `forecast`, `floor`, `rising`, `fast`, `postponed`, or
  `hold` when the window that closed at the last sampling boundary was not measured yet.

## Lead time from pod startup

With `predictivePolicy.scaleUpLeadFromStartup: true`, the scale-up lead follows how fast the workload actually starts. It is the longest time a current pod of the target took from creation to readiness, plus five seconds for the decision and the scale write. `scaleUpLeadSeconds` applies until a start has been observed. This option requires `requestDemand`.

```yaml
predictivePolicy:
  scaleUpLeadSeconds: 15
  scaleUpLeadFromStartup: true
  tolerance: 0.1
```

## Limits

Fast CPU collection still measures executed CPU. Rebuilt demand assumes that waiting requests cost about as much CPU as the ones that finished in the same window, and that adding pods serves them; it does not help when the bottleneck is a database, I/O, a lock or a downstream service.

Earlier versions also had an `arrivals` target, which forecast incoming requests and converted them with a learned CPU cost per request, together with `forecastIntervalSeconds` and `extraMetrics`. They have been removed. A model without `forecastTarget` now forecasts reconstructed CPU.
