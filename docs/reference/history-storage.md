# History storage and ownership

The production reconciler and sampler use `internal/history.Store`. `Load` returns a consistent, caller-owned logical
snapshot and opaque revision; `Commit` conditionally publishes a replacement. Conflicts cause a fresh read and repeat
of the mutation. Backends preserve unrelated data, enforce their limits, and never shorten training windows to fit.
The snapshot's final validation callback rechecks resource identity and caller-supplied freshness after staging I/O.

`prediction.Runtime` provides explicit prediction, pruning and session-lifecycle methods. Its adapter preserves the
existing Python implementations, cancellation and typed warmup errors, and copies forecast inputs before invoking
legacy predictors. `metrics.CPUObservation` carries raw demand/capacity, source identity, expiry and metric status
before evaluator substitutions. The pure decision package consumes explicit inputs; Kubernetes writes stay outside it.

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

Every patch reloads the current object, checks UID/generation/deletion, and uses resourceVersion-based optimistic locking.
Conflict retries repeat the mutation against fresh state; they do not replay a full stale status snapshot. Both loops use
the same total status byte budget, preserving required safety histories rather than silently truncating them.

These checks provide conditional updates, not a cross-object transaction between an HPAPlus specification, history and
Scale. Identity is checked again after journal publication before attempting Scale. Existing Scale resourceVersion checks
and conservative recovery still apply.

## Automatic allocation

`AllocatingStore` is wired in `main.go`. Small histories retain the existing single ConfigMap format and 768 KiB state /
256 KiB journal partitions. When a state write exceeds the small-state partition, the backend automatically creates
immutable ConfigMaps containing ordered 512 KiB binary chunks. A single model can span chunks; all models retain their
configured windows. Allocation follows actual encoded demand, not estimated row sizes or model priority.

The logical state budget is 16 MiB per HPAPlus, including unrelated root values; the journal retains its 256 KiB budget.
Every physical ConfigMap also stays within Kubernetes' 1 MiB data limit. At most 32 chunks describe one committed state.
Existing non-growing oversized journals retain the compatibility allowance. Oversized requests or Kubernetes quota
failures preserve the last committed snapshot and return an explicit storage failure; no model is silently evicted.
ResourceQuota must allow staging a replacement snapshot alongside the committed one, plus the root and any orphan
chunks awaiting cleanup. These are per-resource storage limits, not a global operator memory or cluster-storage budget.

`status.historyCapacity.stateBudgetBytes` reports the selected backend's logical budget: 16777216 for production's
allocating backend, 786432 for the retained single-ConfigMap implementation. Current/projected state bytes refer to
logical serialized history and unrelated values; they exclude chunk API metadata, transport/base64 overhead and staged
copies. `projectedJournalBytes` remains a nominal advisory estimate; actual serialized checks govern writes.

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

The root stores a version-2 manifest containing chunk names, UIDs, SHA-256 digests, byte counts and the pending journal.
Each chunk is immutable and owned by the HPAPlus; a root-UID label scopes cleanup. The logical state inside chunks uses
the existing strict version-1 codec. Readers validate ownership, UIDs, hashes, sizes and journal fields before returning
any state. If publication deletes chunks from an older read, the reader detects the changed root revision and retries.

A monotonically increasing root annotation fences competing writers before cleanup. Cleanup deletes only older,
unreferenced chunks for that root and owner, using UID/resourceVersion preconditions. It does not infer ordering from
opaque Kubernetes resource versions or wall clocks. Stale writers cannot publish after another writer advances the
fence. Failed staging/publication leaves the previous complete snapshot readable; later writes reclaim orphan chunks.
Journal-only updates reuse unchanged state chunks. Removing models reclaims their stored bytes on the next publication.
Deleting the HPAPlus lets Kubernetes garbage collection remove the root and chunks.

Chunk reads and cleanup lists use the uncached API reader. This avoids starting a cluster-wide informer that would
retain all history chunk contents in the operator's cache. Raw metric observation reuse stays process-local: the metrics
API remains the source of measurements, while durable source watermarks prevent duplicate training across restarts or
writers. A distributed raw-observation cache is not required for this policy and has not been added.

## Compatibility and missing data

Migration happens automatically when a write first requires allocation. Once migrated, a resource keeps the manifest
format even after shrinking. Older strict readers also reject the new optional source-marker fields in version-1 model histories.
Older binaries may reject the version-2 root. Do not downgrade a migrated resource to an incompatible operator or use a
single-root backup as a complete backup. Preserve the root **and all referenced chunks**.

A missing or corrupt chunk blocks history mutation and scaling recovery rather than inventing training data or dropping
unknown scale intent. Recreating a deleted chunk gives it a new Kubernetes UID; blindly applying a saved root is
insufficient. Investigate ownership, chunk references and any pending journal before manual recovery. See
[history corruption and recovery](history-recovery.md).

## Competing forecast completions

The native controller queue serializes a namespace/name within each controller. Sampling runs in a separate controller;
there is no global lock across processes. Leader election supplies the deployment's ordinary single active manager,
while optimistic storage/status checks remain necessary during shutdown, takeover and concurrent sampling.

Before fitting, orchestration copies the forecast-owned consumed marker, cache and failure state. Success and failure
publication compare those fields again on every fresh storage read and conflict retry, in addition to owner generation,
target identity, history epoch and input deadline checks. The first completion wins. A late success cannot clear another
completion's failure; a late failure cannot back off another completion's success or increment its failure count. A
rejected success contributes no decision candidate. An uncached result for an already consumed timestamp is also
rejected when no failure state needs clearing. This is a completion check, not a model execution lease.

Sampler appends, other models and journal writes are excluded from the completion comparison and remain preserved.
Pruning/reset helpers invalidate forecast state through epochs; status writes still patch only owned fields against
current resource versions. Multi-replica leader/node failover and cross-object recovery campaigns remain separate from
these deterministic ownership guarantees.
