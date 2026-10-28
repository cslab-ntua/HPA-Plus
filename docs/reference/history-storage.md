# History storage and ownership

The reconciler and the sampler use `internal/history.Store`. `Load` returns a consistent, caller-owned logical
snapshot and an opaque revision; `Commit` conditionally publishes a replacement. A conflict causes a fresh read and a
repeat of the change. Backends keep unrelated data, enforce their limits and never shorten training windows to fit.
The snapshot's final validation callback checks the resource identity and the caller's freshness again after staging.

`prediction.Runtime` provides the prediction, pruning and session-lifecycle methods. It keeps the Python models'
cancellation and typed warm-up errors, and copies forecast inputs before calling a model. `metrics.CPUObservation`
carries raw demand and capacity, source identity, expiry and metric status before any evaluator substitutions. The
decision package works only on explicit inputs; Kubernetes writes stay outside it.

## Field ownership

| State | Owner and concurrency rule |
| --- | --- |
| Target identity and target-change reset | Reconciler or sampler through the same conditional history mutation. Never transfer history across workload UIDs. |
| Training rows, start/reset gates, sample slots and source watermarks | Sampler; rereads after gathering and validates again before publication. |
| History epoch and reset invalidations | Reset helpers, called by either loop. Increment epoch and clear consumed/cache/backoff/source markers together. |
| Consumed marker, cached prediction and failure backoff | Forecast orchestration; generation, target identity and epoch checks preserve concurrently sampled rows. |
| Pending scale journal | Scale writer and recovery; storage treats it as opaque durable intent and never discards it during allocation/cleanup. |
| Scale accounting and reconciliation conditions | Reconciler, using optimistic status patches. |
| `SamplingActive` | Sampler, preserving reconciliation-owned status fields. |
| Root fence, chunk references, immutable chunks | Storage backend only. Neither loop sees ConfigMap revisions or chunk layouts. |

Status ownership is explicit at the field boundary:

| Status fields | Writer |
| --- | --- |
| `conditions[SamplingActive]` | Sampler only; condition-level generation is its own report. |
| `observedGeneration`, all other operator conditions, `modelStatuses`/omitted count, `currentMetrics`/omitted count, `historyCapacity`, `lastDecision` | Reconciliation health publication. Preserve foreign condition types. |
| `targetIdentity`, `currentReplicas`, `desiredReplicas`, `reference`, `lastScaleTime`, `lastScaleDecisionID`, both recommendation histories and both scale-event histories | Reconciliation target binding, Scale publication and journal recovery. |
| Required zero `desiredReplicas`/`reference` on a first status write | Shared patch serialization; either loop can initialize the wire fields without replacing existing values. |

Every patch reloads the current object, checks UID, generation and deletion, and uses resourceVersion-based
optimistic locking. Conflict retries repeat the change against fresh state instead of replaying an old status
snapshot. Both loops share the same total [status size budget](configuration.md#status-size) and keep the required
safety histories instead of truncating them.

These checks give conditional updates, not a transaction across the HPAPlus spec, its history and the Scale
subresource. Identity is checked again after the journal is saved and before the Scale write, and the Scale
resourceVersion checks and conservative recovery still apply.

## Storage layout

Each HPAPlus owns a `hpa-plus-<name>-data` root ConfigMap. Small histories live in it, with a 768 KiB state partition
and a 256 KiB journal partition. When a state write outgrows that partition, the store creates immutable ConfigMaps
holding ordered 512 KiB binary chunks. One model can span chunks, and every model keeps its configured window.
Allocation follows the real encoded size, not estimated row sizes or model priority.

The logical state budget is 16 MiB per HPAPlus, including unrelated root values, and the journal keeps its 256 KiB
budget. Every physical ConfigMap stays within Kubernetes' 1 MiB limit, and at most 32 chunks describe one committed
state. A journal that is already over budget may stay the same size or shrink, but cannot grow. An oversized write or
a Kubernetes quota failure keeps the last committed snapshot and returns an explicit storage failure; no model is
evicted silently. A ResourceQuota must allow a replacement snapshot to be staged next to the committed one, plus the
root and any orphan chunks waiting for cleanup. These are per-resource limits, not a global operator memory or
cluster storage budget.

When training fills the budget, reduce the number of models or their retained history. The sampler applies a reduced
window before gathering, even during delayed starts or missing CPU, and removing all models clears their histories.
Pending decisions and target metadata are kept. Retention settings must still meet each model's minimum history and
cadence. The operator never shortens windows to fit the bytes, because that could stop models from warming up. For an
oversized journal, reduce the stabilization history needed; an unreadable or invalid journal needs repair, see
[history recovery](history-recovery.md). `historyMaxAge` adds age-based retention on top of the counts; see
[training data age](forecast-policy.md#training-data-age).

## Capacity planning status

Reconciliation assesses capacity before evaluating metrics or models, including during scale cooldown and pending
journal recovery. `status.historyCapacity` is a fixed-size advisory report:

| Field | Meaning |
| --- | --- |
| `projectedStateBytes` | Conservative estimate for all configured full training windows, lifecycle metadata, envelope and current unrelated ConfigMap data. |
| `projectedJournalBytes` | Nominal future journal size at configured stabilization windows and sync cadence, at least current retained recommendation counts plus one and current journal bytes. |
| `currentStateBytes` | Normalized encoded stored state and unrelated data, excluding the pending journal. Includes histories awaiting removal. |
| `currentJournalBytes` | Normalized current journal size, including its field framing. |
| `stateBudgetBytes` | Logical state budget: 16777216 bytes. |
| `journalBudgetBytes` | Journal partition budget: 262144 bytes. |

`HistoryCapacityAvailable` reports `WithinBudget`, `ProjectedCapacityExceeded`, `ProjectedJournalCapacityExceeded`,
`CurrentCapacityExceeded` or `NotEvaluated` (Unknown, with no numbers when the assessment is unavailable). A current
overrun takes precedence over a projected one. Reducing the configuration can clear a projected warning on the next
reconciliation. Current usage reflects the history read at the start of the reconciliation; later writes show up in
the next report. Current and projected state bytes count logical serialized history and unrelated values, not chunk
API metadata, transport or base64 overhead, or staged copies.

The projection accounts for JSON escaping in model names, the full configured counts for all five model types, wide
numeric values and cache, backoff and scheduling metadata. It doesn't allocate future sample arrays, and it stops at
9007199254740991 bytes, the largest exactly representable JSON integer; a report at that ceiling means at least that
many estimated bytes. Age limits, delayed starts and unfinished warm-up don't reduce the requested window, and
metadata is reserved even when a model doesn't use every field. Real encoded samples can be smaller, so a projected
warning doesn't prove that writes will fail.

These estimates neither reserve space nor change sampling, fitting, fallback or scaling. Real byte checks still
govern every write. The journal projection uses `ceil(stabilizationWindow/syncPeriod)+1` rows in each direction and
at least the currently retained count plus one, with wide row values, escaped target references and room for IDs
and resource versions. Faster watch-triggered reconciliations, unusually long identities and future unrelated data
can exceed the estimate, so review stabilization windows and sync cadence when journal growth is projected. There is
no eviction.

## Publication and recovery

```mermaid
sequenceDiagram
    participant W as History writer
    participant R as Root ConfigMap
    participant C as Immutable chunk ConfigMaps
    W->>R: Conditional fence increment; retain old manifest
    W->>C: Reclaim older unreferenced chunks
    W->>C: Stage complete replacement state
    W->>W: Recheck identity and caller freshness
    W->>R: Conditional publish of manifest and journal
    W->>C: Reclaim superseded chunks
```

The root stores a version-2 manifest with chunk names, UIDs, SHA-256 digests, byte counts and the pending journal.
Each chunk is immutable and owned by the HPAPlus, and a root-UID label scopes cleanup. The logical state inside the
chunks uses the strict version-1 codec. Readers validate ownership, UIDs, hashes, sizes and journal fields before
returning any state. If a publication deletes chunks from an older read, the reader sees the changed root revision
and retries.

A monotonically increasing root annotation fences competing writers before cleanup. Cleanup deletes only older,
unreferenced chunks for that root and owner, with UID and resourceVersion preconditions. It never infers ordering
from opaque Kubernetes resource versions or wall clocks. A stale writer cannot publish after another writer has
advanced the fence. Failed staging or publication leaves the previous complete snapshot readable, and later writes
reclaim orphan chunks. Journal-only updates reuse unchanged state chunks, removed models free their bytes on the next
publication, and deleting the HPAPlus lets Kubernetes garbage collection remove the root and the chunks.

Chunk reads and cleanup lists use the uncached API reader, so the operator doesn't start a cluster-wide informer
that would keep every chunk in its cache. Raw metric reuse stays inside the process: the metrics API remains the
source of measurements, and durable source watermarks prevent duplicate training across restarts and writers.

## Format changes and missing data

A history moves to the chunked format the first time a write needs it, and stays in that format after it shrinks.
Builds without chunk support cannot read it, and they also reject the optional source-marker fields in version-1
model histories, so don't go back to such a build with migrated data. The root alone is not a complete backup:
keep the root **and all referenced chunks**.

A missing or corrupt chunk blocks history changes and scaling recovery instead of inventing training data or
dropping unknown scale intent. Recreating a deleted chunk gives it a new Kubernetes UID, so applying a saved root
again is not enough. Check ownership, chunk references and any pending journal before a manual recovery; see
[history corruption and recovery](history-recovery.md).

## Competing forecast completions

The controller queue serializes a namespace and name within each controller. Sampling runs in a separate controller,
and there is no global lock across processes. Leader election gives the deployment a single active manager, and the
optimistic storage and status checks still matter during shutdown, takeover and concurrent sampling.

Before fitting, orchestration copies the forecast-owned consumed marker, cache and failure state. Success and failure
publication compare those fields again on every fresh read and conflict retry, along with the owner generation,
target identity, history epoch and input deadline. The first completion wins. A late success cannot clear another
completion's failure, and a late failure cannot back off another completion's success or increase its failure count.
A rejected success adds no decision candidate. An uncached result for a timestamp that was already consumed is also
rejected when there is no failure state to clear. This is a completion check, not a lease on model execution.

Sampler appends, other models and journal writes are left out of the completion comparison and are kept.
Pruning and reset helpers invalidate forecast state through epochs, and status writes still patch only owned fields
against current resource versions.

## Sampling and duplicate measurements

Periodic sampling wakes at a stable per-resource offset, smaller than `min(syncPeriod/10, 1 second)`, after the next
slot. Recorded slot timestamps, earlier lifecycle deadlines and prompt checks after slow I/O are not affected by the
offset.

For timestamped CPU measurements, training stores a hash of the contributing pod names and timestamps and their
oldest source time. A repeated identity, or a source watermark that hasn't advanced, uses up the sampling slot but
adds no training row and doesn't advance the model cadence; `SamplingActive=AwaitingNewObservation` reports this
normal waiting state. Watermarks are stored with the model history and cleared on resets, so a restart cannot add the
same measurement twice.

Sources without usable timestamps use plain slot sampling until a timestamped source has been accepted. Once a model
has a source watermark, untimestamped observations cannot bypass it. `metricMaxAge` also rejects old and future
source times for evaluation. Sparse or stalled sources can delay warm-up or expire an age-bounded window; HPA+ never
makes up measurements to fill the gap.
