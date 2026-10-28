# Decision logs and Kubernetes Events

HPA+ writes structured decision logs and bounded Kubernetes Events. They only report: they never change replica
selection, scale safety checks, retry policy or journal recovery. Logs and Events are best-effort observations; the
HPAPlus status and the scale journal are the accounting records.

## Logs and correlation

The manager defaults to production JSON logging at `info`. Set Helm `logging.level=debug` for per-model execution,
raw CPU forecasts, cache use and cadence details. The chart supports only `info` and `debug`; when running the binary
directly, use the `--zap-log-level` flags. Debug output uses the same error sanitization.

Resource-scoped logs include `namespace`, `resourceName` and a fresh `reconciliationID`. After the resource is read,
they also carry its `resourceUID`, `generation`, `targetAPIVersion`, `targetKind` and `targetName`. Model execution logs
inherit that context and add `modelName`/`modelType`. Prediction adapters use the supplied context instead of a global
logger. Tree forecast debug records retain `diagnosticScope` for locating [feature snapshots](feature-diagnostics.md).

Each evaluated scaling plan emits one `Scale decision` summary after the apply attempt. Its `decisionID` is assigned
before model evaluation and reused in the durable journal and `status.lastScaleDecisionID` when a scale change is
accounted. A no-op decision has an ephemeral ID in its log, because it does not create a scale journal. Interrupted
attempt recovery logs the original journal ID with a new reconciliation ID; recovery never replays the old request.
Failures before a plan can be evaluated have a `Reconciliation failed` record with a phase and safe error category.
Sampling uses its own reconciliation ID and reports errors through the same safe boundary.

| Summary field | Meaning |
| --- | --- |
| `outcome` | `completed` or `failed` for the scale/accounting apply attempt |
| `scaleWrite` | API-write outcome, detailed below |
| `mode` | Candidate composition: `baseline`, `predictive`, `mixed`, or `hold` when `HoldCurrent` kept the current count; separate from the observe/assist/control rollout mode |
| `requestedReplicas`, `observedReplicas` | Workload Scale spec and observed pod counts before the attempt |
| `baselineReplicas` | Reactive metric result, even when it is not selected as a candidate |
| `baselineIncluded` | Whether the baseline participated in selection |
| `selectedReplicas`, `guardedReplicas`, `targetReplicas` | Candidate selection, partial-metric guard, and final constrained request |
| `constraints` | Fixed planner reasons such as partial metrics, stabilization, rate limit and replica bounds |
| `candidates`, `candidatesOmitted` | Up to 64 model/replica pairs in deterministic name order, and the omitted count |
| `errorReason` | Fixed error category; no backend response or arbitrary error text |

`scaleWrite` distinguishes outcomes that must not be conflated:

- `not_attempted`: an owner, freshness, journal or status-budget guard prevented the API call.
- `unchanged`: the desired Scale count already matched; no Scale update was made.
- `accepted`: the Scale API accepted the update. If `outcome=failed`, subsequent accounting or journal cleanup failed;
  it does not mean the scale request was rejected, nor does acceptance mean pods are already ready.
- `rejected`: a conflict, not-found, invalid or forbidden response proved the conditional request was rejected.
- `unknown`: the request failed without proof of rejection, such as a transport timeout. The durable journal is kept
  for conservative accounting; it is unsafe to replay that request blindly.

The baseline has its own field and is not inserted into the model-candidate map, so a user model named `hpa` cannot
hide a baseline or model entry. Logged user-supplied identity fields are capped at 256 UTF-8 bytes and stripped of
control characters. Namespace and resource names are Kubernetes identifiers. The log summary is not stored anywhere
else and says nothing about model quality.

## Error boundary

Controller/sampler errors returned to controller-runtime and errors sent to application loggers expose only
`none`, `canceled`, `timeout`, `forbidden`, `unauthorized`, `conflict`, `not_found`, `invalid`, `throttled`, `unavailable`
or `operation_failed`. The wrapper preserves `errors.Is`/`errors.As`, so retry and safety classification continue to
work. The original error is never formatted into a log field. Model/backend/hook errors, response bodies and filesystem
paths cannot be recovered by switching these application logs to debug. Conditions, model status and the phase/model
correlation fields identify the failed operation; inspect its configuration and dependency health separately.

This contract applies to HPAPlus application reporting and returned reconcile errors. Kubernetes, client-go, the
container runtime and third-party libraries may produce their own infrastructure logs. It does not redact resource
names supplied deliberately by an administrator or turn container-exec access into a data-isolation boundary.

## Events

The manager uses the asynchronous Kubernetes Event recorder with source component `hpa-plus`. Events reference the
HPAPlus UID in its workload namespace. Condition notifications are emitted only after a successful optimistic status
patch, comparing the actual previous and committed states; conflicts, no-op patches, stale generations and restarts
do not invent new transitions. Initial healthy/unknown states are quiet. False transitions produce Warnings; a direct
False-to-True transition produces a Normal recovery. Repeated unchanged failure conditions produce no new Event.

| Condition | Warning reason | Recovery reason |
| --- | --- | --- |
| ConfigurationValid | InvalidConfiguration | ConfigurationRecovered |
| MetricsAvailable | MetricsUnavailable | MetricsRecovered |
| PredictionsAvailable | PredictionsUnavailable | PredictionsRecovered |
| SamplingActive | SamplingUnavailable | SamplingRecovered |
| HistoryCapacityAvailable | HistoryCapacityLow | HistoryCapacityRecovered |
| ScalingActive | ScalingUnavailable | ScalingRecovered |
| ModelStatusComplete | ModelReportsOmitted | ModelReportsRecovered |
| MetricStatusComplete | MetricReportsOmitted | MetricReportsRecovered |

Invalid configuration has a single actionable warning rather than a second ScalingActive warning. Lack of a usable
prediction can include model warmup; it describes baseline fallback, not proof of a model defect. A False-to-Unknown
transition is not reported as recovery. Unknown/custom condition types and conditions for another generation are ignored.
Event messages are fixed guidance strings; condition messages, raw errors and model configuration are never copied.

Additional Normal Events are `ScaleRequested` after an accepted Scale API call and `ScaleAccountingRecovered` after
conservative recovery accounting. `StatusPublicationFailed` is a Warning when health status cannot be published.
No-op, rejected and ambiguous Scale requests do not emit `ScaleRequested`. Event delivery failure never rolls back or
blocks an accepted scale request. These Kubernetes Events are distinct from status's rate-policy accounting histories.

Suppression is **per HPAPlus UID and reason for five minutes**, shared by controller and sampler. Failure and recovery
reasons have separate suppression keys. A recreated resource gets a new identity. A process-wide limiter allows five
notifications per second with a burst of ten, bounding floods across resources and key churn. The suppression map is
capped at 1,024 entries and evicts the oldest; the global limiter still applies after eviction. Wall-clock regression
does not reset an existing key's suppression interval. Client-go also applies its recorder correlation/queue limits.
Suppressed/dropped notifications are not queued for replay; Events are not an exhaustive audit log. Local suppression
resets on process replacement, while persisted condition comparison still suppresses unchanged states.

For Events the operator ClusterRole grants only **create/patch on core Events**, across workload namespaces. Events
are reported with leader election disabled too. The namespaced election Role contains only Lease grants, and there is
no Event read, update or delete permission and no Secret permission. See [RBAC](rbac.md).

```sh
kubectl get events -n workloads --field-selector involvedObject.kind=HPAPlus,involvedObject.name=checkout
kubectl logs -n hpa-plus deployment/hpa-plus-operator -c hpa-plus-operator
```

## Rollout evidence

Decision JSON includes `rolloutMode`, `baselineDesiredReplicas` and `controlDesiredReplicas`.
`scaleWrite=observed` means an observe cycle completed without issuing Scale or hypothetical accounting.
`status.lastDecision` holds the bounded latest snapshot. The `mode` log and metric label describes candidate
composition, not the rollout policy. See [rollout modes](../user-guide/rollout.md).
