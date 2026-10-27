# Operator observability

Use [resource status](../reference/configuration.md#health-and-model-status) to explain one autoscaler's current
configuration, metric availability, model eligibility and retry state. The operator also exposes aggregate Prometheus
metrics on its existing `/metrics` endpoint, using port 8080 by default (`--metrics-bind-address`).

The Helm chart does not create a metrics Service or ServiceMonitor. Configure your scraper's pod discovery for port
8080 (and port 8082 for isolated worker telemetry). The [operations guide](operations.md) explains setup and response. For a local inspection, replace `hpa-plus` below with the operator's installation namespace:

```sh
kubectl -n hpa-plus port-forward deployment/hpa-plus-operator 8080:8080
# In a second terminal:
curl http://localhost:8080/metrics
```

## Metrics and interpretation

| Metric | Labels | Meaning |
| --- | --- | --- |
| `hpaplus_metric_evaluations_total` | `outcome` | Collection/evaluation attempts, from both reconciliation and history sampling. |
| `hpaplus_metric_evaluation_duration_seconds` | `outcome` | Histogram of collection/evaluation time. |
| `hpaplus_model_runs_total` | `family`, `outcome` | Actual forecast executions, before result persistence or final eligibility checks. |
| `hpaplus_model_run_duration_seconds` | `family`, `outcome` | Histogram including executor queue waits and hooks within the forecast call. |
| `hpaplus_model_evaluations_total` | `family`, `state` | Eligibility assessments after processing models, including cache use, warmup and backoff. |
| `hpaplus_sampling_checks_total` | `outcome` | Per-resource sampling checks, including slots that are not due. |
| `hpaplus_sampling_check_duration_seconds` | `outcome` | Histogram of time spent checking/sampling one resource. |
| `hpaplus_history_operations_total` | `operation`, `outcome` | History ConfigMap I/O attempts, including rejected writes, retries and no-ops. |
| `hpaplus_history_operation_duration_seconds` | `operation`, `outcome` | Histogram of time spent in one storage attempt. |
| `hpaplus_history_configmap_data_bytes` | `operation` | Histogram of combined data/binaryData bytes read or proposed for writing. |
| `hpaplus_worker_active` | `kind`, `activity` | Gauge of active child processes or persistent exchanges; returns to zero after cleanup. |
| `hpaplus_worker_events_total` | `kind`, `event` | Starts, exits, failed exits, resets and session-limit rejections; exposes worker churn. |
| `hpaplus_worker_queue_wait_seconds` | `kind`, `stage` | Histogram of slot/session acquisition and exchange-gate wait, including cancellation. |
| `hpaplus_history_sample_age_seconds` | `family`, `edge` | Age of oldest/newest valid retained CPU sample at model evaluation; not a current-state gauge. |
| `hpaplus_decision_constraints_total` | `reason` | Constraints affecting completed decisions; multiple reasons may count for one decision. |
| `hpaplus_status_budget_rejections_total` | `partition` | Complete-status or pre-scale accounting writes refused for size. |
| `hpaplus_rollout_evaluations_total` | `mode` | Completed observe/assist/control cycles; observe never writes Scale or scale-accounting histories. |
| `hpaplus_decisions_total` | `mode`, `direction` | Completed, accounted decisions, including holds; excludes recovery and failed writes. |

Histogram metrics expose the usual `_bucket`, `_sum` and `_count` series. Labels use fixed categories:

- Metric outcomes: `complete`, `partial`, `unavailable`, `canceled`.
- Model run outcomes: `success`, `insufficient_history`, `failed`, `timeout`, `canceled`.
- Model families: `linear`, `holt_winters`, `arima`, `xgboost`, `lightgbm`.
- Model states: the states documented for `status.modelStatuses`.
- Sampling outcomes: `not_due`, `completed`, `duplicate_source`, `missing_cpu`, `failed`, `canceled`.
- History operations: `read`, `write`, `create`; outcomes: `success`, `noop`, `failed`, `conflict`, `capacity`, `invalid`, `ownership`, `canceled`.
- Worker kinds: `one_shot`, `persistent`; activity: `processes`, `exchanges`; events: `started`, `exited`, `failed`, `reset`, `limit_rejected`.
- Worker wait stages: `slot`, `session`, `exchange`; history edges: `oldest`, `newest`.
- Decision constraints: `assist_baseline`, `partial_metrics`, `stabilization`, `rate_limit`, `disabled`, `minimum`, `maximum`.
- Status partitions: `complete`, `accounting`.
- Rollout modes: `observe`, `assist`, `control`. Decision modes below describe candidate composition, a separate concept.
- Decision modes: `baseline`, `predictive`, `mixed`; directions: `up`, `down`, `hold`.

Unexpected categories collapse to `other`. Resource names, namespaces, model names, hook addresses and error strings
are never metric labels. New series appear on their first observation; counters reset when the operator process restarts.

A successful model execution may later be excluded because its result could not be persisted or expired before use.
Cache/backoff checks increment eligibility counts without incrementing execution counts. `completed` sampling includes
concurrent no-ops; it is not an exact count of appended training rows. A completed scale decision remains counted when
only the subsequent health-status publication fails. Direction compares requested replicas, not observed running pods.

Storage counters count individual attempts, so a conflict followed by success increments both outcomes. A `read`
includes get-or-create and decoding; creation also records its own attempt. Byte observations include unrelated data
and binary values and rejected proposals; unchanged writes do not observe size again. This histogram is not a gauge
of current per-resource usage. Inspect `SamplingActive` and operator logs to identify an affected resource.
For advance sizing, inspect `HistoryCapacityAvailable` and `status.historyCapacity` on that resource. The report compares
estimated full-window state and current state/journal sizes with their budgets. `ProjectedCapacityExceeded` is an
advisory warning; actual storage rejection is still reported separately by the sampler. See
[capacity planning status](../reference/configuration.md#capacity-planning-status) for assumptions and limitations.

## Example queries

Model execution outcomes per second:

```promql
sum by (family, outcome) (rate(hpaplus_model_runs_total[5m]))
```

95th-percentile model execution time, in seconds:

```promql
histogram_quantile(0.95,
  sum by (le, family) (rate(hpaplus_model_run_duration_seconds_bucket[5m])))
```

Completed baseline-only decisions per second:

```promql
sum(rate(hpaplus_decisions_total{mode="baseline"}[5m]))
```

Baseline decisions are expected when no model is configured, during warmup or when no forecast is usable. Inspect
model eligibility and resource status to distinguish those cases. Eligibility counters record assessment events rather
than the current number of resources in each state. Controller-runtime's existing reconciliation/work-queue metrics
remain available alongside these metrics. The independent sampler uses the `history-sampler` controller name, so its
queue and reconciliation metrics can be inspected separately from the main HPAPlus controller.

Rejected storage writes per second:

```promql
sum by (outcome) (rate(hpaplus_history_operations_total{operation="write",outcome!~"success|noop"}[5m]))
```

Worker starts/exits/resets expose process churn; a reset event counts a live session explicitly reset, not every
future restart or every fallback request. Session acquisition includes process creation when needed, whereas exchange
wait measures only the persistent request gate. Process gauges are local to one operator, not a cluster-wide total.
Decision constraints are counted only after successful accounting, alongside the completed decision counter. Observe cycles
are excluded from both; the separate rollout counter and latest-decision status report them.
Keep worker endpoints distinct and scope monitoring queries to the intended cluster and installation.

## Model execution timing

`status.modelStatuses` optionally includes `lastAttemptTime`, `lastSuccessTime`, `lastRunDurationMilliseconds` and
`lastRunOutcome`. Times refer to runtime attempts; duration includes hooks, executor queues and the forecast call,
not subsequent status/history writes. Outcomes are the fixed model-run categories above. Success means the runtime
returned successfully; the result may still be excluded by history, persistence or expiry checks. Use model `state`
and `forecastReplicas` for eligibility. These diagnostics never authorize scaling.

Within the same observed generation, skips, cache reuse and backoff retain the last recorded timing. Failed attempts
update attempt/outcome/duration while preserving the previous success time. New specifications clear old timings.
Timings are best-effort status: interrupted/unpublished attempts and reports omitted by the 16 KiB budget may be absent.
There is no additional durable attempt journal or per-model Prometheus label.

## Complete status budget

Every controller status mutation checks the encoded JSON size against **256 KiB**. This includes all typed status
fields, conditions and recommendation/event histories; it is separate from the object specification and metadata.
Existing oversized status may shrink or stay the same size, permitting repair and recovery, but may not grow.
Neither safety history nor foreign conditions are silently removed.

Before a new Scale write, projected accounting must fit **192 KiB**, reserving 64 KiB for diagnostic reports and health
metadata. The controller checks before journal publication and again afterwards against freshly read status. Holds
also validate accounting before recording recommendations. Projection uses the same accounting function as commit.
A rejected decision does not send Scale; `ScalingActive=False/StatusCapacityExceeded` explains the hold when health
can be published, and the budget-rejection metric remains available if status itself is too full to report health.

Reduce configured stabilization/policy retention or allow old entries to expire; preserve accounting during repair.
A journal from an already applied Scale remains until accounting succeeds. An independent actor can still fill status
between the final preflight and Scale—different Kubernetes resources cannot commit atomically—so the durable journal
and recovery path remain necessary. This budget does not impose a CRD-wide admission limit on other status writers.

## Decision logs and Events

HPA+ provides production JSON decision summaries, correlated model/sampler logs and rate-limited Kubernetes Events.
Set Helm `logging.level=debug` for per-model details; `info` is the default. Each summary distinguishes an accepted
Scale request from a rejected, unchanged or unknown write, and its decision ID matches journal accounting. Condition
failure/recovery Events refer to the HPAPlus in its workload namespace and never copy backend error text.
See [decision reporting](../reference/decision-reporting.md) for fields, interpretation, Event reasons and limits.
