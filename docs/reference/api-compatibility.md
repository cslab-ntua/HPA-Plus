# API version and defaults

HPA+ serves one API version, **`hpa.plus/v1alpha1`**, which is also the storage version, so there is no conversion
webhook. The history ConfigMaps and the worker protocol have their own version numbers, separate from the API.

## What is public

| Surface | Contract |
| --- | --- |
| HPAPlus `spec` | User configuration. Field names, units and the meaning of explicit zero and false values are stable. |
| HPAPlus `status` | Written only by the controller. `lastDecision` is a bounded report, not a command. Status ownership and accounting use optimistic patches. |
| History ConfigMaps and the scale journal | Internal formats with their own versions and strict validation; see [history storage](history-storage.md). They are not part of the API, even though their Go types live in the API package. |
| Stored `replicaHistory` rows | Field names are kept as they are. Models train on `totalCpuUsageMillicores`; replica counts are context and accounting, not the training signal. |
| Go `TimestampedSample` | Demand samples with replica context, or replica-only recommendation and event rows. `TimestampedReplicas` is a deprecated alias for Go code. |
| Worker protocol | Versioned separately; see [worker protocol](worker-protocol.md). |

## Defaults

The controller's defaults live in `api/v1alpha1/defaults.go`. The decision logic, the baseline and cadence, ARIMA
retention and every model adapter's deadline use them. `HPAPlusSpec.WithDefaults` returns a deep copy with omitted
fields filled in and never changes the live resource. The API server doesn't write defaults into stored objects, so
manifests read back the way they were applied.

| Field | Omitted value |
| --- | --- |
| `mode` | `control`; set `observe` explicitly for a new trial |
| `minReplicas` | 1 |
| `syncPeriod` | 15000 milliseconds |
| `cpuInitializationPeriod`, `initialReadinessDelay` | 300 and 30 seconds |
| `tolerance` | 0.1 |
| `decisionType`, `includeHPA` | maximum, false |
| `predictionFailurePolicy` | Reactive |
| `behavior.scaleUp` | 0-second stabilization, Max selection, 100% or four pods per 15 seconds (the Kubernetes default) |
| `behavior.scaleDown` | 0-second stabilization, Max selection, 100% per 60 seconds |
| Model `perSyncPeriod`, `calculationTimeout` | 1 and 30000 milliseconds for every family |
| ARIMA `historySize` | 50 |
| Model `predictivePolicy.tolerance` | 0.1 when `predictivePolicy` is set; explicit 0 disables it |

The Kubernetes HPA defaults its scale-down stabilization window to 300 seconds, while HPA+ uses zero. Set
`behavior.scaleDown.stabilizationWindowSeconds: 300` to get the Kubernetes behavior. Predictive tolerance only applies
inside a `predictivePolicy`; without the policy, forecasts are used as single values.

Explicit zero, false and empty values are never mistaken for omitted ones or changed silently. Invalid explicit
values stay invalid and must pass validation. A policy list you set for one `behavior` direction replaces that
direction's default list; the two are not merged.

No metric is added for you. A missing model list becomes an empty one, which leaves only the reactive
recommendation; models still need the right metric configured. Optional freshness, reuse and age policies stay off
unless set, and so do ARIMA trend and other library-dependent choices. Per-model numerical defaults and validation
live in each model's implementation and the pinned Python runtime.
