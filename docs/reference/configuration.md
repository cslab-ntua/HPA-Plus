# Configuration

An `HPAPlus` lives in the same namespace as the workload it scales. [Getting started](../user-guide/getting-started.md)
has a complete example. These are its spec fields:

| Field | Default | Meaning |
| --- | --- | --- |
| [`scaleTargetRef`](#scaletargetref) | required | The workload to scale |
| [`mode`](#mode) | `control` | `observe`, `assist` or `control` |
| [`minReplicas`](#minreplicas) | `1` | Lowest replica count |
| [`maxReplicas`](#maxreplicas) | required | Highest replica count |
| [`metrics`](#metrics) | required | HPA metric specs for the reactive recommendation; models need a CPU utilization metric |
| [`models`](#models) | required, can be empty | Forecasting models |
| [`behavior`](#behavior) | see below | Stabilization windows and rate limits |
| [`syncPeriod`](#syncperiod) | `15000` ms | How often HPA+ samples and decides |
| [`metricMaxAge`](#metric-source-freshness) | no limit | Maximum age of metric timestamps |
| [`cpuInitializationPeriod`](#cpuinitializationperiod) | `300` s | Pod start period when CPU samples may be skipped |
| [`initialReadinessDelay`](#initialreadinessdelay) | `30` s | Pod start period for readiness changes |
| [`tolerance`](#tolerance) | `0.1` | Smallest metric ratio change that scales |
| [`decisionType`](#decisiontype) | `maximum` | How candidates are combined |
| [`includeHPA`](#includehpa) | `false` | Whether the reactive recommendation joins the forecasts |
| [`predictionFailurePolicy`](#predictionfailurepolicy) | `Reactive` | What happens when no forecast is usable |

## scaleTargetRef

```yaml
scaleTargetRef:
  apiVersion: apps/v1
  kind: Deployment
  name: application
```

The workload that HPA+ scales, in the same namespace as the HPAPlus. HPA+ reads and updates its Scale subresource
and measures the pods it selects. The operator's permissions cover Deployments, ReplicaSets, StatefulSets and
ReplicationControllers. `kind` and `name` are required. Without `apiVersion`, HPA+ uses the API server's preferred
version for that kind.

Pointing the HPAPlus at another workload, or recreating the workload, starts a new training history.

Required; there is no default.

## mode

```yaml
mode: observe
```

`observe` gathers metrics and runs the models but never changes the replica count. `assist` scales but never below
the reactive HPA recommendation. `control` applies the configured decision. See [rollout modes](../user-guide/rollout.md).

Default value: `control`. Set `observe` explicitly for a new trial.

## minReplicas

```yaml
minReplicas: 2
```

The lower limit for the number of replicas to which the autoscaler can scale down to. `minReplicas` is allowed to be 0
if at least one Object or External metric is configured. Scaling is active as long as at least one metric value is
available.

Default value: `1`.

## maxReplicas

```yaml
maxReplicas: 15
```

The upper limit for the number of replicas to which the autoscaler can scale up.
It cannot be less than minReplicas.

Required; there is no default. Must be at least `1`.

## syncPeriod

```yaml
syncPeriod: 10000
```

Equivalent to `--horizontal-pod-autoscaler-sync-period`; the frequency with which the HPA+ calculates replica counts and
scales in milliseconds.

Set in milliseconds.

Default value: `15000` (15 seconds).

Set in milliseconds.

History sampling uses watches and schedules each resource's next slot, with earlier wake-ups for model lifecycle
deadlines. Slot timestamps retain whole-second precision, so subsecond values do not produce subsecond training
observations. Fractional-second slots are rounded down consistently before persistence. Invalid and model-free
resources stop periodic checks after successful reporting/cleanup; spec changes wake them again.

## cpuInitializationPeriod

```yaml
cpuInitializationPeriod: 150
```

Equivalent to `--horizontal-pod-autoscaler-cpu-initialization-period`; the period after pod start when CPU samples
might be skipped.

Set in seconds.

Default value: `300` (5 minutes).

## initialReadinessDelay

```yaml
initialReadinessDelay: 45
```

Equivalent to `--horizontal-pod-autoscaler-initial-readiness-delay`; the period after pod start during which readiness
changes will be treated as initial readiness.

Set in seconds.

Default value: `30` (30 seconds).

## tolerance

```yaml
tolerance: 0.25
```

Equivalent to `--horizontal-pod-autoscaler-tolerance`; the minimum change (from 1.0) in the desired-to-actual metrics
ratio for the horizontal pod autoscaler to consider scaling.

Default value: `0.1`.

## decisionType

```yaml
decisionType: mean
```

Decider on which evaluation to pick if there are multiple models provided.

Possible values:

- **maximum** - pick the highest evaluation of the models.
- **minimum** - pick the lowest evaluation of the models.
- **mean** - calculate the mean number of replicas (rounded to nearest integer) between the models.
- **median** - calculate the median number of replicas between the models.

Default value: `maximum`.

## includeHPA

```yaml
includeHPA: true
```

Controls whether the baseline Horizontal Pod Autoscaler (HPA) calculation participates alongside available predictions.
When `includeHPA` is `false` (the default), a cycle with model predictions uses those predictions alone. If no model
provides a usable prediction in the current cycle, HPA+ uses the HPA baseline. By default this includes warmup,
intentional cadence skips, and model failures. Set a model's optional `predictionMaxAge` (milliseconds) to allow
reuse of a successful prediction within its sample-age and forecast-horizon bounds; see [models](../user-guide/models.md).

Fresh production forecasts also expire at their data-relative target when caching is disabled. Choose a horizon
longer than sample age plus fitting time; stale forecasts leave the baseline available. See
[forecast validity and uncertainty](forecast-policy.md).

## predictionFailurePolicy

```yaml
predictionFailurePolicy: HoldCurrent
```

What HPA+ does when no model has a usable forecast, for example during warm-up, after a failed fit or when a forecast
has expired:

- **Reactive** - use the reactive HPA recommendation from `metrics`.
- **HoldCurrent** - keep the currently requested replica count.

The rest of the decision still applies: stabilization, rate limits, `minReplicas`, `maxReplicas` and the scale-down
hold for incomplete metrics. [Request-demand](../user-guide/request-demand.md) models require `HoldCurrent`.

Default value: `Reactive`.

## behavior

Scaling behavior to apply, with the same fields and rules as the Kubernetes HPA `behavior`. See the
[Horizontal Pod Autoscaler docs](https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/#configurable-scaling-behavior).

Default value: both stabilization windows `0` seconds. Scale-up allows 100% or four pods, whichever is more, every
15 seconds. Scale-down allows 100% every 60 seconds.

## models

List of statistical models to apply.
See [the models section for details](../user-guide/models.md).

## metrics

List of metrics to target for evaluating replica counts. At least one is required; the operator does not add a
default CPU metric. Every prediction model requires a CPU resource metric with target type `Utilization` and a
positive `averageUtilization`.
See [the metrics section for details](../user-guide/metrics.md).

## Validation and upgrades

The CRD rejects scalar bounds and enum violations during create/update. Cross-field constraints are checked by the
controller, preserving support for Kubernetes 1.23 without CEL or an admission webhook. An object rejected by the
controller remains stored, but the controller stops scaling and the sampler skips it until its configuration is fixed.
The controller logs the validation error and reports it through the `ConfigurationValid` condition.

`syncPeriod` and model `calculationTimeout` must be positive, representable millisecond durations. CPU initialization
and readiness delays must be non-negative, representable second durations. `tolerance` must be finite and non-negative.
Model names must be nonblank and unique, with exactly one configuration block matching the model type. Optional
`startInterval` and `resetDuration` must be positive. Optional model `historyMaxAge` is a positive representable
millisecond duration and must allow the configured training count at the nominal sampler cadence; see
[age retention](../user-guide/models.md#training-history-age-retention).

The [model constraints](../user-guide/models.md#configuration-constraints) now reject configurations that previously
failed at prediction time or never accumulated enough retained history to run. Before upgrading, correct resources
with duplicate/blank model names, invalid numeric bounds, incompatible ARIMA modes, or unreachable history/cadence
settings. Schema-invalid existing resources may also need correction before a subsequent spec update can succeed.

## Partial metric availability

If at least one configured metric fails to gather or evaluate, usable metrics and forecasts may still scale up, but
HPA+ holds the requested replica count against downscale recommendations. This applies to model-only decisions too.
Configured minimum/maximum replica bounds remain enforced. When every metric fails, scaling waits for a successful
retry. Invalid or overflowing gathered metric observations reject the current calculation. All supported metric families
check quantities and arithmetic bounds; raw conversion failures count as unavailable metrics. `spec.tolerance` applies
consistently to every supported metric type, including Pods and Resource AverageValue.

## Health and model status

`status.observedGeneration` identifies the specification generation assessed by the controller. Conditions report:

| Type | Meaning |
| --- | --- |
| `ConfigurationValid` | Whether controller-side validation passed. |
| `MetricsAvailable` | Whether all configured metrics were gathered and evaluated safely. Partial availability restricts downscale. |
| `PredictionsAvailable` | Whether at least one model supplied a usable forecast at the last evaluation. |
| `ScalingActive` | Whether reconciliation completed; failure reasons identify the failed phase. |
| `ModelStatusComplete` | Whether every model fits in the detailed status budget. |
| `MetricStatusComplete` | Whether every gathered metric can be included in the detailed status budget. |
| `SamplingActive` | The independent sampler’s last outcome for this generation. |

`SamplingActive` is owned by the sampler; it does not advance the reconciliation-owned `status.observedGeneration`.
A persisted CPU sampling attempt reports `True/SamplesPersisted`; missing CPU reports `False/CPUUnavailable`.
Storage failures report `HistoryReadFailed`, `HistoryWriteFailed`, `HistoryCapacityExceeded`, `HistoryDataInvalid`
or `HistoryOwnershipMismatch`. Other failures identify the phase, and an expired attempt reports `SamplingTimedOut`.
Invalid configurations report `False/InvalidConfiguration`; no configured models report `Unknown/NoModelsConfigured`.
Checks for slots that are not due retain the last outcome for the same generation, including failures; a new generation
starts at `Unknown/AwaitingSample` until an attempt completes. This is a last-outcome condition, not a freshness heartbeat.
Each attempt has 30 seconds, followed by a separate three-second status budget if the manager remains active.
Manager shutdown cancels both; it does not publish a failure for deliberate shutdown. Unchanged status is not rewritten.

`status.modelStatuses` reports each model's last assessed state, CPU history/required sample counts, usable
`forecastReplicas` (including zero), `consumedUntil`, optional cache `expiresAt`, and failure count/`retryAfter`.
States are `Waiting`, `WarmingUp`, `Scheduled`, `Ready`, `Cached`, `Backoff`, `Failed`, `InvalidHistory`, `Unavailable`
and `Expired`. `Cached` can coexist with retry metadata when a failed model still has an eligible cached forecast.
Detailed reports are limited to 64 entries and 16 KiB; `modelStatusesOmitted` counts excluded models. This reporting
limit does not disable model execution. Backend and hook error text stays in operator logs.

During cooldown, metric and model reports retain the last evaluation for the same generation. Check explicit expiry
when interpreting cached results; conditions are not continuous freshness checks. A changed generation clears old
reports until the controller evaluates the new configuration.

`status.currentMetrics` contains measured values before conservative missing-pod estimates. Resource metrics report
average quantity and, for utilization targets, utilization percentage. Pods report average quantity; Object/External
Value targets report total value. Object/External AverageValue targets report total divided by observed replicas,
rounded up to milli-units; at zero replicas the report contains only total `value` because no per-pod average exists.
The independent reporting budget is 64 entries and 16 KiB, with `currentMetricsOmitted` counting gathered values
that could not be included. This budget does not restrict scaling evaluation. Unavailable metrics have no fabricated
status values; use `MetricsAvailable` to distinguish a partial gather from reporting-budget omissions.

## Metric source freshness

Optional `spec.metricMaxAge` limits the age of source metric timestamps, in milliseconds. For example:

```yaml
spec:
  metricMaxAge: 60000
```

Choose a limit that accommodates your metric provider's reporting interval and normal latency. The supported range is
1–9223372036854 ms. When unset, HPA+ preserves its existing behavior without a source-age limit. This setting is separate
from a model's `predictionMaxAge`, which controls reuse of an already generated forecast.

With the policy enabled, a missing, future or expired source timestamp makes that metric source unavailable. Each raw
value is checked before aggregation, so a fresh first value cannot hide stale later values. A partially available set
can still scale up; downscaling is held. If no metric remains usable, the controller retries without scaling or sampling.
Freshness is also checked after prediction, during scale-journal retries and after journal persistence, and before history
writes. A slow model or storage retry can therefore require another read. Requests already sent to the API server are
not an atomic transaction with source expiry.

Training history already stored before enabling this policy is retained. Sampling still uses wall-clock slots; repeated
provider observations that remain within the age limit can currently appear in multiple slots. Source-sample
deduplication remains separate work.

Sampling and reconciliation can reuse a completed raw metric read for at most `min(syncPeriod, 1000)` milliseconds,
measured from collection start. This local reuse limit does not replace `metricMaxAge` or change its opt-in default.
Reuse requires matching resource identity/specification and Scale state; configured source expiry is checked again
on reuse. Failed/partial reads and observations with missing/future timestamps are not cached. Each caller evaluates
its own copy. Simultaneous misses gather independently, so a canceled or blocked reconciliation cannot hold up sampling.
The process-local cache is bounded to 256 entries and 16 MiB encoded data, with a 256 KiB per-entry limit. There are no
new public configuration fields for these fixed conservative limits.

## History storage capacity

Each resource owns a `hpa-plus-<name>-data` root ConfigMap. Small histories use the original 768 KiB state /
256 KiB journal partitions. Production automatically allocates immutable 512 KiB chunk ConfigMaps when state outgrows
that partition, up to 16 MiB logical state per HPAPlus. Every model retains its configured training window; the journal
keeps its 256 KiB budget and each physical ConfigMap stays within Kubernetes' 1 MiB limit.

These are fixed operator limits. Oversized proposals and quota failures preserve committed data; a journal that cannot
fit is rejected before scaling. Additional quota is needed to stage a replacement alongside the committed snapshot.
See [storage architecture, ownership and migration](history-storage.md) before backing up or downgrading a migrated
resource. Aggregate storage metrics are described in [observability](../user-guide/observability.md).

Reduce model count or configured retained history when training fills its budget. The sampler applies reduced windows
before gathering, even during delayed starts or missing CPU; removing all models clears their histories. It preserves
pending decisions and target metadata. Retention settings must still satisfy each model’s minimum history and cadence.
The operator does not silently shorten windows to fit bytes, because doing so could prevent models from warming up.
For an oversized journal, reduce stabilization history requirements; unreadable or invalid journals require repair.
See [history corruption and recovery](history-recovery.md) before repairing persisted state, especially a pending scale journal.

The retained single-ConfigMap backend allows each oversized legacy partition to remain unchanged or shrink. Production
can migrate larger state to chunks; a non-growing oversized journal retains its compatibility allowance. Optional per-model `historyMaxAge` adds age retention.

### Capacity planning status

Reconciliation assesses capacity before evaluating metrics or models, including during scale cooldown and pending
journal recovery. `status.historyCapacity` is a fixed-size advisory report:

| Field | Meaning |
| --- | --- |
| `projectedStateBytes` | Conservative estimate for all configured full training windows, lifecycle metadata, envelope and current unrelated ConfigMap data. |
| `projectedJournalBytes` | Nominal future journal size at configured stabilization windows and sync cadence, at least current retained recommendation counts plus one and current journal bytes. |
| `currentStateBytes` | Normalized encoded stored state and unrelated data, excluding the pending journal. Includes histories awaiting removal. |
| `currentJournalBytes` | Normalized current journal size, including its field framing. |
| `stateBudgetBytes` | Logical state budget: 16777216 bytes for automatic allocation; 786432 for the single-ConfigMap backend. |
| `journalBudgetBytes` | Journal partition budget: 262144 bytes. |

`HistoryCapacityAvailable` reports `WithinBudget`, `ProjectedCapacityExceeded`, `ProjectedJournalCapacityExceeded`, `CurrentCapacityExceeded`, or
`NotEvaluated` (Unknown, with no numeric report when assessment is unavailable). A current partition overrun takes
precedence over a projected one. A configuration reduction can clear a projected warning on the next reconciliation.
Current usage reflects the history read at the beginning of reconciliation; later writes appear in subsequent reports.

The projection accounts for JSON escaping in model names, full configured counts for all five model families,
wide numeric values and cache/backoff/scheduling metadata. It does not allocate future sample arrays and saturates
at 9007199254740991 bytes for extremely large totals (the largest exactly representable JSON integer, for compatibility
with supported API servers). A report at that ceiling means at least that many estimated bytes. Age limits, delayed starts and incomplete warmup do not reduce the requested
window. Metadata is reserved even when a model is not currently using every field. Actual encoded samples can be
smaller, so a projected warning does not prove that writes will fail.

These estimates neither reserve space nor change sampling, fitting, fallback or scaling decisions. Actual byte checks
still govern every write. Journal projection uses ceil(stabilizationWindow/syncPeriod)+1 rows in each direction and
at least the currently retained count plus one. It includes wide row values, escaped target references and allowances
for IDs/resource versions. Faster watch-triggered reconciliations, unusually long identity values and future unrelated
data can exceed the estimate. Review stabilization windows and sync cadence when journal growth is projected.
Automatic chunk allocation is implemented; eviction is deliberately excluded. Non-Kubernetes backends remain optional
extensions. See [history recovery](history-recovery.md) for corruption handling and [model runtime](model-runtime.md)
for worker controls and sizing considerations.

Model execution has fixed [training/search work limits](worker-protocol.md#training-and-search-work-budgets). Exceeding
one produces a failed forecast through existing backoff/fallback handling; it does not silently trim a training window
or change model parameters. Reduce the requested history, tree complexity or ARIMA search/order as appropriate.


### Sampling source identity and jitter

Periodic sampling wakes at a stable per-resource offset smaller than `min(syncPeriod/10, 1 second)` after the next
slot. Recorded slot timestamps, earlier lifecycle deadlines and prompt checks after slow I/O remain unchanged.

For timestamped CPU measurements, training stores a hash of contributing pod names/timestamps and their oldest source
time. A repeated identity or a source watermark that has not advanced consumes the sampling slot but adds no training
row and does not advance model cadence. `SamplingActive=AwaitingNewObservation` reports this normal waiting state.
Watermarks persist with model history and are cleared on resets, so restarts cannot re-ingest the same measurement.
The raw observation cache remains local to the process; no distributed raw-metric cache is required.

Legacy sources without usable timestamps retain slot sampling until a timestamped source has been admitted. Once a
model has a source watermark, untimestamped observations cannot bypass it. `metricMaxAge` additionally enforces source
age/future-time rejection for evaluation. Sparse or stalled sources may delay warmup or expire an age-bounded window;
HPA+ does not fabricate intermediate measurements to fill it.


## Forecast runtime controls

`holtWinters.lookAhead` is optional, positive milliseconds (maximum 9223372036854). It selects the first discrete
forecast point at or beyond the requested horizon after the newest sample. Omission retains the historical one-step
forecast. The Go adapter sends the ordered timestamps with CPU values; explicit horizons cannot use timestamp-free
legacy series. Cache lifetime uses the explicit horizon when present. See [model semantics](../user-guide/models.md)
for the irregular-spacing approximation and freshness policy.

Operator-wide process/session limits and retry circuit controls are configured with Helm `workers` and `modelRetry`
values or the corresponding binary flags. See [model runtime](model-runtime.md) for defaults, sizing and compatibility.


## Complete status capacity

Controller writes enforce 256 KiB of encoded status; pre-scale accounting reserves 64 KiB of that for diagnostics.
Long stabilization windows and event histories can exhaust the remaining 192 KiB. Decisions are then held with
`StatusCapacityExceeded` rather than dropping safety history. See [status budgeting](../user-guide/observability.md#complete-status-budget)
for legacy no-growth handling, hold decisions and recovery behavior.

## Rollout policy and compatible defaults

`spec.mode` accepts `observe`, `assist` or `control`. Omitted/null retains legacy control behavior. Observe reports
counterfactuals without a new Scale write; assist floors selection at the reactive baseline before normal safeguards.
See [rollout controls and decision status](../user-guide/rollout.md) for promotion, concurrency and accounting semantics.
`status.lastDecision` records the latest bounded plan and write outcome, independently of durable journal accounting.

Stable defaults are centralized in the API package. See
[API compatibility](api-compatibility.md) for preserved wire names, explicit zero/false behavior and migration/version rules.
