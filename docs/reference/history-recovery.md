# History corruption and recovery

Each HPAPlus owns a `hpa-plus-<name>-data` ConfigMap in its namespace. Its JSON document contains training histories,
model runtime metadata and an optional pending scale journal. Recovery must keep the journal: a failed API response
does not prove that a Scale update failed to take effect.

Read [history storage](history-storage.md) for the storage layout and format rules.

## Behavior

| Stored state | Operator behavior |
| --- | --- |
| Version-2 allocated root | Validate all referenced chunk identities/digests and assemble the logical snapshot. Missing/corrupt chunks block mutation and require investigation with a complete backup. |
| Valid unversioned history | Read as unversioned data; written as schema version 1 on the next change. An omitted history epoch means zero. |
| Invalid JSON, unsupported version, duplicate/unknown fields, oversized payload or incompatible field types | Reject the document before mutation. History reads, sampling and scaling fail; original bytes remain available for inspection. |
| Missing/null required journal fields or missing/null replica counts in journal recommendation rows | Reject the document. Null recommendation lists are valid; null rows are not. Explicit zero replica counts remain valid. |
| Cached forecast object with missing/null `replicas` | Reject the document, preventing a fabricated zero-replica prediction. An omitted/null cache remains valid. |
| Explicit null or negative history epoch | Reject the document. No automatic epoch reassignment is attempted. |
| Decodable journal with invalid identity, counts, timestamps or recommendations | Reconciliation preserves it without clearing, accounting or replaying. Sampling can continue only if the document is otherwise readable; it preserves the journal. |
| Invalid CPU training window (such as unordered, duplicate, missing/zero timestamps or negative CPU) | Exclude the affected model; sampler resets that window, increments its epoch and clears cache, retry and consumed/sample progress. Other histories and the journal survive. |
| Otherwise valid history with future row, sample-slot or source timestamps | Exclude forecasts and pause sampling with `HistoryClockSkew`; preserve history, epoch, cache and journal. Retry until wall time catches up. Investigate clock synchronization; explicitly repair genuinely incorrect future timestamps. Structural corruption still follows the reset rule above. |
| Valid epoch already at MaxInt64 when a reset is required | Fail without wrapping or partially resetting the model. |
| Training rows without CPU | Sampler removes them; they do not satisfy CPU warm-up requirements. |
| Structurally readable cache with incompatible fingerprint, invalid timestamps/count or expired deadline | Cache is unavailable; the usual model eligibility and fallback rules apply. |
| Invalid or incompatible failure-backoff metadata | Does not suppress a new fit. A later success/reset clears it, and a later failure can replace it. |
| Actual state/journal byte budget exceeded | Reject growth; preserve existing data. Partitions already over budget can stay the same size or shrink within the absolute 1 MiB limit. |

Wire validation checks that safety-sensitive scalar values are present before Go defaults can hide missing data. It
is separate from semantic validation: a decodable object is not necessarily eligible for prediction or journal
recovery. The operator uses the decoding, training-window and journal validation in `internal/history`.

## Persisted field policy

These rules cover schema version 1 and unversioned data. Wrong JSON types, integer overflow, duplicate keys and
unknown fields are rejected throughout the typed document, and malformed timestamp strings are decoding failures.
"Readable" below does not mean a record is eligible for forecasting or recovery.

| Record and fields | Omission, null and semantic policy |
| --- | --- |
| Envelope `schemaVersion` | Omitted or explicit 0 means unversioned; 1 is current. Null, other versions and non-integer values are rejected. Normal writes or explicit migration set 1. |
| Envelope `targetIdentity` | Omitted/null/empty means no target is bound yet. Live target adoption resets training before use; a changed workload identity cannot reuse old training. |
| Envelope `modelHistories` | Omitted/null means an empty map. Empty or null model entries decode as zero state; their missing/mismatched type causes initialization from the configured model, not a usable prediction. |
| Envelope `pendingScale` | Omitted/null means no pending record. A present journal must satisfy the wire and recovery rules below. |
| Model `historyEpoch` | Omitted means zero; explicit null/negative is rejected. Reset increments the epoch; MaxInt64 cannot reset. |
| Model `type` | Omitted/null/empty or a type different from the configured family is not eligible for that model. Sampler initializes the matching type. |
| Model `syncPeriodsPassed` | Omitted/null means zero. Negative progress invalidates the window. Kept for compatibility; cadence uses sample timestamps, not this counter. |
| Model `replicaHistory` | Omitted/null is an empty window. Rows without CPU, including null rows, cannot train models and are removed by sampling. CPU-backed rows follow the sample checks below. |
| Model `startTime` | Optional/null means no persisted start gate. Valid future timestamps intentionally delay sampling/model use. Reset-window preserves this field. |
| Model `lastSampleTime`, `lastModelRunTime` | Optional/null means unset. Present sample markers must be nonzero and not future; consumed markers must also have retained CPU history and not exceed its latest timestamp. Invalid markers invalidate the window. |
| Model `lastObservationID`, `lastObservationTime` | Optional pair of source markers; omitted/null means no marker. A nonempty marker needs a 32-byte hexadecimal identity and a valid, nonfuture RFC3339Nano time. Malformed or incomplete pairs invalidate the window. A window reset clears both; cache/backoff-only repairs keep them. |
| Model `cachedPrediction`, `failureState` | Optional/null means absent. Present objects follow the cache/backoff rules below; no missing scalar may create a usable cached zero prediction. |
| Training row `time`, `totalCpuUsageMillicores` | Rows without CPU are not training data. CPU-backed rows require nonzero, nonfuture, strictly increasing times and nonnegative CPU. Violations invalidate the whole model window. |
| Training row `replicas` | Omission/null means zero; negative counts invalidate a CPU-backed window. This recorded count is not the predicted count: models forecast CPU and convert using current capacity. |
| Training row `metric`, `requestPerPodMillicores`, `targetCpuUtilizationPercentage` | Optional; null is allowed. When present on CPU-backed rows, utilization must be finite/nonnegative and request/target strictly positive. |
| Cache `replicas` | Must be explicitly present and non-null. Explicit zero is valid. Negative values make the cache unavailable. |
| Cache `fingerprint` | Omitted/null/empty cannot match the live nonempty fingerprint. The fingerprint binds configuration, workload/session identity and current capacity. |
| Cache `consumedUntil`, `completedAt`, `expiresAt` | Missing/null/zero is unusable. Consumption cannot follow completion; completion cannot be future; expiry must follow consumption. Actual reuse is bounded by current policy, history readiness and training age, even if a stored deadline is later. |
| Cache `historyStart` | May be omitted/null when age retention is off. Age retention requires a nonzero timestamp no later than consumption and uses it to bound reuse. |
| Backoff `fingerprint`, `consecutiveFailures`, `failedAt`, `retryAfter` | Missing/null/invalid metadata cannot suppress a fit. Require matching fingerprint, positive count, nonzero/nonfuture failure time, and retry after failure within the configured maximum (default five minutes; configurable up to 24 hours) plus serialization rounding. Expired backoff does not suppress a fit. |
| Journal `id`, `ownerUID`, `targetUID`, `resourceVersion` | Required non-null wire values and nonblank semantic identity. Compare live owner/target and Scale resource version during recovery. |
| Journal `target` (`kind`, `name`, `apiVersion`) | Target object is required/non-null. Kind and name must be nonblank valid path segments. API version may be omitted/null and is then discovered; when supplied it must be a valid group/version. Incomplete references preserve the journal without recovery. |
| Journal `previousDesired`, `desired`, `observed` | Required non-null int32 counts, all nonnegative; desired must differ from previous desired. Explicit zero is supported. |
| Journal `time` | Required/non-null, nonzero and not future at recovery. Clock skew can postpone recovery; it never justifies deleting a journal. |
| Journal `scaleUpRecommendations`, `scaleDownRecommendations` | Both fields must exist; null/empty lists are supported. Each row requires explicit non-null replicas >= 0 and a nonzero time no later than the decision. Null rows are invalid. Other optional sample metadata is not used for stabilization accounting. |

Invalid training rows also include negative recorded replica counts or nonpositive capacity context. A window repair
clears samples, cadence progress, sample and consumed markers, cache and backoff, and increments the epoch. It keeps
the model type, start gate, other models, target identity and pending journal. A corrupt journal blocks recovery and
any repair; its missing fields must never be inferred or discarded.

## Inspection and manual repair

1. Inspect `SamplingActive`, `ScalingActive`, model status and operator logs. `HistoryDataInvalid` identifies rejected
   stored data; a capacity warning alone is advisory. See [capacity planning](history-storage.md#capacity-planning-status).
2. Save the complete HPAPlus, history ConfigMap and current workload Scale state before editing anything. Keep owner UID,
   resource versions, raw JSON, unrelated data and binary data with the snapshot. Restore only against the intended
   resource incarnation, using an optimistic resource-version check so concurrent work is not overwritten.
3. Locate the failure. Missing replica values cannot be inferred as zero. Unknown schema versions or fields may belong
   to a newer operator; use a compatible version or an explicit migration instead of deleting unfamiliar fields.
4. If only derived cache metadata is damaged, removing that model's optional `cachedPrediction` can allow a fresh fit
   under normal readiness/cadence rules. Preserve the training window, epoch and pending journal. This is a manual
   repair option, not an automatic operator action.
5. For damaged journal identity or replica counts, restore known-correct data only from trusted evidence. Do not
   delete the journal to unblock reconciliation: that may discard required rate-limit/stabilization accounting for a
   Scale write that already happened. Reapplying the journal's desired count is also unsafe.
6. Once a readable, valid journal is restored, normal recovery resolves it. It clears records already accounted for,
   superseded by another owner/target, or proven unattempted by the workload resource version. Otherwise it conservatively
   accounts the attempted change once using the current Scale state, then clears the record. It never replays the update.
7. Observe the next reconciliations and sampling attempts. Check that conditions recover, affected models warm up or fit,
   and scale-event accounting has not duplicated. Keep the original snapshot for investigation.

There is no whole-store reset or automatic reconstruction of missing journal fields.
