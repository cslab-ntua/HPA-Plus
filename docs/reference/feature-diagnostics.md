# Tree feature diagnostics

Feature diagnostics are available for XGBoost and LightGBM. Diagnostics are **disabled by default**. When enabled by the operator
administrator, they retain one CSV snapshot per resource/model identity in the container that executes Python.
They are temporary debugging data, not a historical training-data archive or a forecast-quality benchmark.

## Enable and retrieve

```yaml
featureDiagnostics:
  enabled: true
  maxFiles: 64
  maxBytes: 16777216
  maxAgeSeconds: 3600
```

Set these Helm values during a normal configuration upgrade. The chart configures `/tmp/hpaplus-features` inside the
existing bounded ephemeral scratch volume. With `workerIsolation.enabled=true`, only `model-workers` receives these
settings; otherwise the manager receives them. No persistent volume, API permission, CRD field or network endpoint
is added. Enabling diagnostics requires the updated source-built image; an older image does not implement this option.

List snapshots in the isolated worker, using your release namespace:

```sh
kubectl exec -n hpa-plus deployment/hpa-plus-operator -c model-workers -- ls /tmp/hpaplus-features
```

For direct execution use `-c hpa-plus-operator`. Retrieve a selected CSV using `kubectl exec ... -- cat <path>` and
redirect stdout to your local file. Snapshots contain CPU and optional auxiliary-metric training features; grant
container-exec access only to operators who may read those measurements. No tokens, model configuration, resource
names or raw session IDs are written into the CSV. Feature column names match the training matrix, including
the one-lag zero-difference column. Up to the **most recent 256 training rows** are included.

Each filename is `<family>-<diagnosticScope>.csv`. The tree forecast debug log includes `diagnosticScope` so
you can match a model's log to its snapshot. The scope is the SHA-256 digest of the controller-owned session identity:
namespace, HPAPlus name and UID, generation, model name, history epoch and resolved target identity. A recreated
resource, changed model configuration, new history epoch or changed target therefore gets a separate destination.
Old destinations are retired by age or capacity. Hashes provide safe filenames; they are not an authorization boundary.
Set Helm `logging.level=debug` when retrieving this per-model log detail; normal decision summaries remain
at `info`. See [decision reporting](decision-reporting.md).
Missing identity skips the write. Linear, Holt-Winters and ARIMA do not have these tree-feature dumps.

## Bounds and lifecycle

| Bound | Default | Supported range |
| --- | --- | --- |
| Snapshot count per executing container | 64 | 1–256 |
| Total published CSV bytes per executing container | 16 MiB | 1–64 MiB |
| Snapshot maximum age | 1 hour | 60 seconds–24 hours |
| Rows per CSV | 256 | Fixed |
| Bytes per CSV, including header | 1 MiB | Fixed |
| Idle cleanup interval | 60 seconds | Fixed |

Writes replace a scope's previous snapshot atomically. Oldest snapshots are evicted to admit a new snapshot within
the aggregate file/byte limits. During replacement, at most one additional 1-MiB staging file can exist per directory,
plus a small lock file; provision scratch storage above `maxBytes` for this overhead and normal model scratch usage.
The existing emptyDir and container ephemeral-storage limits still apply. With several replicas, budgets apply per pod.

Age and count/byte limits are enforced on writes and by an independent idle janitor at startup and every minute.
Deleted resources and removed models need no later forecast to expire. Normally, expiry is bounded by the configured
age plus the cleanup interval and a five-second sweep deadline. Lock contention or an unavailable filesystem can delay
cleanup; the next sweep retries. Files dated in the future are discarded to avoid retaining diagnostics indefinitely
after a backward clock jump. Pod deletion clears its scratch volume; a container restart runs startup cleanup.
Disabling the Helm option changes the pod template; normal replacement drops the old pod's ephemeral diagnostics.

The janitor uses a short-lived, standard-library-only Python process with a 64-MiB address-space ceiling and a
five-second deadline. It performs no numerical imports, forecasting or Kubernetes calls. It runs on standby replicas
as well, because each replica owns its local files. Shutdown cancels and joins cleanup.

The directory is private (`0700`), files are `0600`, and writers/janitors share a nonblocking process lock. A busy
writer causes another diagnostic write to be skipped instead of delaying its forecast. Root/lock symlinks, unsafe
permissions and hard-linked snapshots are rejected. Operations use an opened directory descriptor and do not follow
snapshot symlinks. Only recognized snapshots and abandoned staging files are removed; cleanup does not recurse or
touch unrelated files. The administrator must use a dedicated local directory with trusted ancestors. These controls
do not isolate mutually hostile processes sharing the same Unix UID.

Malformed diagnostic settings, full or unavailable storage and write/cleanup failures do not fail a forecast. Errors
use fixed messages without paths or values. Optional diagnostics can be missing after contention, eviction or I/O
failure; never treat them as an audit log or as a complete record of model executions.

## Direct and legacy use

For local execution set `HPAPLUS_FEATURE_DUMP_DIR` to a dedicated absolute directory with an existing parent.
The runtime creates the final directory privately. Optional environment limits are
`HPAPLUS_FEATURE_DUMP_MAX_FILES`, `HPAPLUS_FEATURE_DUMP_MAX_BYTES` and `HPAPLUS_FEATURE_DUMP_MAX_AGE_SECONDS`.
The Go adapters provide `diagnosticScope` automatically when the controller has assigned a session identity.
Direct Python callers must supply their own 64-character lowercase hex `diagnosticScope` in the tree input.
The operator's one-shot envelope and private Unix transport remain version 1; the new input field is optional.

**Migration:** `XGB_FEATURE_DUMP` and `LGBM_FEATURE_DUMP` are no longer honored. They used a single shared path per
algorithm family. Replace them with the directory setting and an explicit identity; there is no silent unscoped
fallback. Standalone Python invocations enforce retention on writes. For idle standalone directories, schedule the
same cleanup command yourself (the manager and worker-server already schedule it):

```sh
HPAPLUS_FEATURE_DUMP_DIR=/absolute/private/diagnostics python algorithms/feature_diagnostics.py
```
