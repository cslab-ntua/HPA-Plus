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
| [`metricMaxAge`](#metricmaxage) | no limit | Maximum age of metric timestamps |
| [`cpuInitializationPeriod`](#cpuinitializationperiod) | `300` s | Pod start period when CPU samples may be skipped |
| [`initialReadinessDelay`](#initialreadinessdelay) | `30` s | Pod start period for readiness changes |
| [`tolerance`](#tolerance) | `0.1` | Smallest metric ratio change that scales |
| [`decisionType`](#decisiontype) | `maximum` | How candidates are combined |
| [`includeHPA`](#includehpa) | `false` | Whether the reactive recommendation joins the forecasts |
| [`predictionFailurePolicy`](#predictionfailurepolicy) | `Reactive` | What happens when no forecast is usable |

Omitted fields are filled in when the controller reads the resource, not stored in it; see
[API version and defaults](api-compatibility.md).

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

The lowest replica count HPA+ scales down to. It can be 0 if at least one Object or External metric is configured.
Scaling stays active as long as at least one metric value is available.

Default value: `1`.

## maxReplicas

```yaml
maxReplicas: 15
```

The highest replica count HPA+ scales up to. It cannot be less than `minReplicas`.

Required; there is no default. Must be at least `1`.

## metrics

The metrics used for the reactive recommendation, with the same specs as the Kubernetes HPA. At least one is
required; the operator does not add a default CPU metric. Every prediction model requires a CPU resource metric with
target type `Utilization` and a positive `averageUtilization`. See [metrics](../user-guide/metrics.md).

## models

The forecasting models; see [models](../user-guide/models.md). The field is required but can be an empty list, which
leaves only the reactive recommendation.

## behavior

Scaling behavior to apply, with the same fields and rules as the Kubernetes HPA `behavior`. See the
[Horizontal Pod Autoscaler docs](https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/#configurable-scaling-behavior).

Default value: both stabilization windows `0` seconds. Scale-up allows 100% or four pods, whichever is more, every
15 seconds. Scale-down allows 100% every 60 seconds. The Kubernetes HPA uses a 300-second scale-down window by
default; set `behavior.scaleDown.stabilizationWindowSeconds: 300` to match it.

## syncPeriod

```yaml
syncPeriod: 10000
```

How often HPA+ samples history and calculates replica counts, in milliseconds, like
`--horizontal-pod-autoscaler-sync-period`.

Default value: `15000` (15 seconds).

The sampler schedules each resource's next slot and wakes earlier for model lifecycle deadlines. Slot timestamps
have whole-second precision, so values below a second don't give sub-second training samples; fractional seconds are
rounded down. Invalid resources and resources without models stop periodic checks after reporting and cleanup, and a
spec change wakes them again.

## metricMaxAge

```yaml
metricMaxAge: 60000
```

Limits the age of source metric timestamps, in milliseconds. Pick a limit that fits your metric provider's reporting
interval and normal latency. The range is 1–9223372036854 ms. This is separate from a model's `predictionMaxAge`,
which controls reuse of a forecast.

Default: no limit.

When set, a missing, future or expired source timestamp makes that metric source unavailable. Every raw value is
checked before aggregation, so a fresh first value cannot hide stale later ones. A partly available set can still
scale up, while scale-down is held. If no metric is usable, the controller retries without scaling or sampling.
Freshness is checked again after forecasting, during scale-journal retries, after the journal is saved and before
history writes, so a slow model or storage retry can need another read. Requests already sent to the API server are
not an atomic transaction with source expiry. Turning `metricMaxAge` on doesn't remove history that is already
stored.

Sampling and reconciliation can reuse a completed raw metric read for at most `min(syncPeriod, 1000)` milliseconds
from the start of the collection, with or without `metricMaxAge`. Reuse needs the same resource identity, spec and
Scale state, and `metricMaxAge` is checked again on reuse. Failed or partial reads and observations with missing or
future timestamps are not cached. Each caller evaluates its own copy, and simultaneous misses gather independently,
so a canceled or blocked reconciliation cannot hold up sampling. The process-local cache holds at most 256 entries
and 16 MiB of encoded data, with 256 KiB per entry. These limits are fixed.

## cpuInitializationPeriod

```yaml
cpuInitializationPeriod: 150
```

Like `--horizontal-pod-autoscaler-cpu-initialization-period`: the period after a pod starts during which its CPU
samples may be skipped.

Set in seconds.

Default value: `300` (5 minutes).

## initialReadinessDelay

```yaml
initialReadinessDelay: 45
```

Like `--horizontal-pod-autoscaler-initial-readiness-delay`: the period after a pod starts during which readiness
changes count as initial readiness.

Set in seconds.

Default value: `30` (30 seconds).

## tolerance

```yaml
tolerance: 0.25
```

Like `--horizontal-pod-autoscaler-tolerance`: the minimum change, from 1.0, in the desired-to-actual metric ratio
before HPA+ scales.

Default value: `0.1`.

## decisionType

```yaml
decisionType: mean
```

How HPA+ picks one count when there are several candidates.

Possible values:

- **maximum** - the highest candidate.
- **minimum** - the lowest candidate.
- **mean** - the mean of the candidates, rounded to the nearest integer.
- **median** - the median of the candidates.

Default value: `maximum`.

## includeHPA

```yaml
includeHPA: true
```

Whether the reactive HPA recommendation from `metrics` joins the model forecasts as one more candidate. With `false`,
a cycle with usable forecasts uses the forecasts alone. When no model has a usable forecast, for example during
warm-up, in a skipped cycle or after a failure, `predictionFailurePolicy` decides what happens.

Set a model's `predictionMaxAge` to reuse a successful forecast within its sample-age and horizon limits, and choose
a horizon longer than the sample age plus the fitting time. See [expiry and reuse](forecast-policy.md#expiry-and-reuse).

Default value: `false`.

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

## Validation

The CRD schema rejects scalar bounds and enum violations when an object is created or updated. The controller checks
the cross-field rules, so HPA+ works on Kubernetes 1.23 without CEL or an admission webhook. An object that fails
these checks stays stored, but the controller doesn't scale it and the sampler skips it until it is fixed. The reason
is logged and reported in the `ConfigurationValid` condition.

`syncPeriod` and model `calculationTimeout` must be positive, representable millisecond durations. CPU initialization
and readiness delays must be non-negative, representable second durations. `tolerance` must be finite and
non-negative. At least one metric is required. Model names must be nonblank and unique, with exactly one
configuration block matching the model type. Optional `startInterval` and `resetDuration` must be positive. Optional
model `historyMaxAge` must be a positive representable millisecond duration that allows the configured training
count at the sampler cadence; see [training data age](forecast-policy.md#training-data-age). The per-model rules are
in [models](../user-guide/models.md#validation).

## Partial metric availability

If at least one configured metric fails to gather or evaluate, usable metrics and forecasts may still scale up, but
HPA+ holds the requested replica count against scale-down. This applies to model-only decisions too. The configured
minimum and maximum still apply. When every metric fails, scaling waits for a successful retry. Invalid or
overflowing metric observations reject the current calculation. All supported metric types check quantities and
arithmetic bounds, and raw conversion failures count as unavailable metrics. `spec.tolerance` applies to every
supported metric type, including Pods and Resource AverageValue.

## Status

`status.observedGeneration` identifies the spec generation the controller assessed. Conditions report:

| Type | Meaning |
| --- | --- |
| `ConfigurationValid` | Whether controller-side validation passed. |
| `MetricsAvailable` | Whether all configured metrics were gathered and evaluated safely. Partial availability restricts scale-down. |
| `PredictionsAvailable` | Whether at least one model supplied a usable forecast at the last evaluation. |
| `ScalingActive` | Whether reconciliation completed; failure reasons identify the failed phase. |
| `ModelStatusComplete` | Whether every model fits in the detailed status budget. |
| `MetricStatusComplete` | Whether every gathered metric fits in the detailed status budget. |
| `SamplingActive` | The independent sampler's last outcome for this generation. |
| `HistoryCapacityAvailable` | Whether stored history fits its storage budget; see [capacity planning](history-storage.md#capacity-planning-status). |

`SamplingActive` is owned by the sampler and doesn't advance the reconciliation-owned `status.observedGeneration`. A
saved CPU sample reports `True/SamplesPersisted`, and missing CPU reports `False/CPUUnavailable`. Storage failures
report `HistoryReadFailed`, `HistoryWriteFailed`, `HistoryCapacityExceeded`, `HistoryDataInvalid` or
`HistoryOwnershipMismatch`. Other failures name the phase, and an attempt that runs out of time reports
`SamplingTimedOut`. Invalid configurations report `False/InvalidConfiguration`, and resources without models report
`Unknown/NoModelsConfigured`. Checks for slots that are not due keep the last outcome for the same generation,
failures included; a new generation starts at `Unknown/AwaitingSample` until an attempt completes. This is a
last-outcome condition, not a freshness heartbeat. Each attempt has 30 seconds, then a separate three-second status
budget if the manager is still active. Manager shutdown cancels both without publishing a failure, and unchanged
status is not rewritten.

`status.modelStatuses` reports each model's last assessed state, CPU history and required sample counts, usable
`forecastReplicas` (zero included), `consumedUntil`, the optional cache `expiresAt`, and the failure count and
`retryAfter`. States are `Waiting`, `WarmingUp`, `Scheduled`, `Ready`, `Cached`, `Backoff`, `Failed`,
`InvalidHistory`, `Unavailable` and `Expired`. `Cached` can come with retry metadata when a failed model still has an
eligible cached forecast. Detailed reports are limited to 64 entries and 16 KiB, and `modelStatusesOmitted` counts
the models left out. This limit doesn't stop models from running. Backend and hook error text stays in the operator
logs. [Observability](../user-guide/observability.md#model-execution-timing) describes the timing fields.

During cooldown, metric and model reports keep the last evaluation for the same generation. Check the explicit
expiry when reading cached results; conditions are not continuous freshness checks. A new generation clears old
reports until the controller evaluates the new spec.

`status.currentMetrics` holds measured values before conservative estimates for missing pods. Resource metrics report
the average quantity and, for utilization targets, the utilization percentage. Pods report the average quantity, and
Object or External Value targets report the total. Object or External AverageValue targets report the total divided
by the observed replicas, rounded up to milli-units; at zero replicas only the total `value` is reported, because no
per-pod average exists. The report is limited to 64 entries and 16 KiB, and `currentMetricsOmitted` counts gathered
values that didn't fit. This limit doesn't restrict scaling. Unavailable metrics get no made-up values; use
`MetricsAvailable` to tell a partial gather from values left out of the report.

`status.lastDecision` records the latest plan and its write outcome; see
[latest decision report](../user-guide/rollout.md#latest-decision-report).

### Status size

Every status write checks the encoded JSON against **256 KiB**. That covers the typed status fields, conditions and
the recommendation and event histories, but not the spec or metadata. A status that is already too large may shrink
or stay the same size, so it can be repaired, but it cannot grow. Safety history and conditions owned by other
writers are never removed silently.

Before a new Scale write, the projected accounting must fit in **192 KiB**, which leaves 64 KiB for reports and health.
The controller checks before saving the journal and again afterwards against freshly read status. Holds also check
the accounting before recording recommendations, using the same accounting function as the real commit. A rejected
decision sends no Scale write: `ScalingActive=False/StatusCapacityExceeded` explains the hold when health can be
published, and `hpaplus_status_budget_rejections_total` counts it even when the status is too full to report it.

Long stabilization windows and event histories can use up the space. Reduce stabilization and policy retention, or
let old entries expire, and keep the accounting during repairs. A journal from an applied Scale write stays until its
accounting succeeds. Another writer can still fill the status between the final check and the Scale write, because
different Kubernetes resources cannot commit together, so the durable journal and recovery are still needed. This
budget is not an admission limit for other status writers.
