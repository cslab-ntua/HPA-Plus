# API compatibility and explicit configuration defaults

HPA+ serves **one served/storage API, `hpa.plus/v1alpha1`**, and separates its evolution from history-store and worker
protocol versions. This change cleans up internal vocabulary and centralizes stable runtime defaults without renaming
serialized fields or introducing a conversion webhook. Keeping one served version avoids an untested dual-version
conversion surface while the public schema's wire names remain compatible.

## Public and internal boundaries

| Surface | Compatibility contract |
| --- | --- |
| HPAPlus `spec` | User configuration. New optional `mode` has enum observe/assist/control; omission/null retains control. Existing wire names, units and explicit zero/false meanings remain. |
| HPAPlus `status` | Controller observations/accounting. `lastDecision` is bounded diagnostic evidence, not a writable command API. Preserve status ownership and accounting through optimistic patches. |
| `HPAPlusData`, model histories and scale journal | Internal ConfigMap formats with their own versioned strict codec/migration rules. They are not public spec fields, even though Go types currently share the API package. |
| Go `TimestampedSample` | Accurate shared name for demand samples with replica context, or replica-only recommendation/event rows. `TimestampedReplicas` remains a deprecated type alias for source compatibility. |
| Persisted `replicaHistory`, `replicas`, CPU fields | Historical wire names remain unchanged. Predictions consume `totalCpuUsageMillicores`; historical replica counts are context/accounting, not the training signal. No bytes need migration for the Go rename. |
| Forecast protocol | Versioned separately in [the worker contract](worker-protocol.md); this change does not alter Python envelopes or units. |

The old Go alias remains assignable, retains deepcopy methods and serializes exactly as before, including large int64
CPU values. Production Go call sites use the clearer name. Older strict history readers still have the limitations
already documented for source markers and v2 chunks; the alias does not remove those earlier downgrade constraints.

## Default ownership

Stable controller defaults now live in `api/v1alpha1/defaults.go`. Decision behavior, controller baseline/cadence,
ARIMA retention validation/pruning and every model adapter's deadline reference this contract. `HPAPlusSpec.WithDefaults`
returns a deep copy, preserves supplied values and fills stable omissions; it never mutates the live resource.
Admission does **not** materialize these defaults. Existing manifests remain unchanged on read/update.

| Field | Omitted value |
| --- | --- |
| `mode` | `control` for legacy compatibility; set observe explicitly for a new trial. |
| `minReplicas` | 1 |
| `syncPeriod` | 15000 milliseconds |
| `cpuInitializationPeriod`, `initialReadinessDelay` | 300 and 30 seconds |
| `tolerance` | 0.1 |
| `decisionType`, `includeHPA` | maximum, false |
| `behavior.scaleUp` | 0-second stabilization, Max selection, 100% or four pods per 15 seconds (the Kubernetes default) |
| `behavior.scaleDown` | 0-second stabilization, Max selection, 100% per 60 seconds |
| Model `perSyncPeriod`, `calculationTimeout` | 1 and 30000 milliseconds for every family |
| ARIMA `historySize` | 50 |
| Model `predictivePolicy.tolerance` | 0.1 when the timed predictive policy is enabled; explicit 0 disables it |

Downscale stabilization now defaults to zero seconds, including existing resources that omit
`behavior.scaleDown.stabilizationWindowSeconds`. To retain the previous five-minute window,
set that field explicitly to `300`. Other explicit windows remain unchanged. Predictive
tolerance defaults only within an enabled `predictivePolicy`; omission of the policy still
preserves scalar forecasting.

Explicit zero, false and empty policies are never mistaken for omissions or silently repaired. Invalid explicit values
remain invalid and must pass validation. Partially configured behavior preserves the existing replacement semantics:
a supplied policy list replaces that direction's default list, rather than merging entries.

No metric is invented. Missing model lists normalize to an explicit empty list for reactive-only declarative manifests;
models still require the appropriate metric to be configured. Optional freshness/reuse/age policies remain absent,
and ARIMA trend/library-dependent tuning choices remain absent. Per-family numerical defaults and validation remain
in their typed model implementation and pinned runtime. These defaults do not freeze every future numerical-library choice.
