# Model runtime and limits

The operator runs Python in bounded processes: a short-lived process for each one-shot forecast, and retained
processes for incremental ARIMA sessions. Both enter `algorithms/worker_limits.py` before loading numerical
libraries. The wrapper sets the soft and hard Linux `RLIMIT_AS` limits together, keeping any stricter inherited hard
ceiling. An allocation beyond the ceiling fails inside that process. A failed exchange evicts and reaps its
persistent worker, so a later attempt starts a clean process, and a one-shot failure releases its concurrency slot.
Input and output limits, process-group cancellation, reaping, per-model deadlines and the
[work budgets](#training-and-search-budgets) all apply.

A persistent reset keeps its admission slot until the old child has been reaped. Admission checks cancellation again
after waiting for the session registry, so a canceled waiter cannot start a replacement child. The Unix client can
return at its deadline before the server has finished canceling, so a client timeout alone doesn't mean server-side
cleanup is done.

| Helm value | Binary flag | Default | Range/meaning |
| --- | --- | --- | --- |
| `workers.memoryMiB` | `--worker-memory-mib` | 1024 | Hard virtual address space per process; 64..65536 MiB, or 0 to disable. |
| `workers.concurrency` | `--worker-concurrency` | 4 | 1..64 simultaneous one-shot processes; queued calls share their model deadline. |
| `workers.sessions` | `--worker-sessions` | 16 | 1..64 retained incremental processes. An idle session still holds its model. |
| `modelRetry.initial` | `--model-retry-initial` | `0s` | 0 follows syncPeriod; otherwise at least 1s and at most the ceiling. |
| `modelRetry.maximum` | `--model-retry-max` | `5m` | 1s..24h; zero selects the default five minutes. |

The binary rejects invalid limits at startup. Helm also checks numeric ranges and duration syntax; the binary checks
the cross-field rules and exact duration bounds. Numerical-library threads are forced to one in both runners,
including BLAS, OpenMP, MKL, BLIS and NumExpr, whatever the host settings. The supported platform is Linux. Use the
Python files from the same build as the manager binary.

## Retry circuit behavior

Each failed fit opens that model's circuit immediately. The next eligible reconciliation at or after `retryAfter` is
the next probe; it can run later, because model cadence and reconciliation scheduling still apply. Failed probes
double the delay up to the ceiling, and a successful fit clears the circuit. Failures are tied to the model's
fingerprint and survive controller restarts. Warm-up and parent cancellation don't count as failures, and changes to
capacity, spec or identity, or a window reset, invalidate the old state. Sampling and other models continue. A model
is never disabled permanently.

The retry policy is fixed for a running operator and applies to all its resources; a change takes effect on restart.
A stored retry stays valid within the new maximum, even if the initial delay changed, and new failures use the new
policy. A stored delay above the new ceiling is treated as invalid and allows a fresh attempt. All leaders should use
the same configuration. Retry times round up to whole seconds, so serialization cannot shorten the wait. An
incremental failure can still try a one-shot run within the original model deadline; only the final failed result
advances the circuit.

## Forecast step limit

ARIMA/SARIMA, XGBoost, LightGBM and Holt-Winters with an explicit `lookAhead` reject requests that need more than
10,000 future sampling steps. The step count uses the median positive sample spacing (at least 1 ms), rounded up. For
example, with one-second samples a horizon above 10,000 seconds is rejected; faster sampling reaches the limit
sooner. The horizon is never shortened silently: reduce `lookAhead` if the logs report the step budget. The failure
follows the normal backoff and fallback rules. Linear evaluates one future point, and Holt-Winters without
`lookAhead` forecasts one step.

The versioned one-shot path checks the horizon before running the model, and direct entry points check it before
building features, searching parameters or fitting. Incremental workers check fits before fitting and appends
against the timestamps they would retain, so a change in sampling cadence can reject an append with an unchanged
configuration; the previous session stays usable. Forecast-only calls are checked too, and a rejection returns no
prediction. This limit comes on top of the process timeout and does not bound training memory.

## Training and search budgets

Shared Python checks reject work before building features, constructing a numerical model or extending an
incremental one. The versioned dispatcher checks before loading the model, and the command-line entry points and the
persistent worker check their own paths. A rejection contains `training budget exceeded`, returns no forecast and
keeps the committed worker state. The normal failure, backoff and fallback rules apply, and configurations are never
reduced silently.

| Work proxy | Maximum |
| --- | --- |
| History rows per fit or incremental append batch, all models | 10,000 |
| Tree matrix cells | 1,000,000 |
| Tree estimators | 2,000 |
| Tree depth | 16; LightGBM still accepts -1 with its leaf cap |
| LightGBM leaves | 65,536 |
| Tree matrix cells × estimators | 200,000,000 |
| Automatic ARIMA fits, including baseline/grid and final selected fit | 256 |
| ARIMA state dimension proxy | 128 |
| ARIMA history rows × state dimension squared | 2,000,000 |

For a rolling window `w`, the CPU feature width is `w + max(1,w-1) + 2`, and auxiliary metrics add `w+2`. Matrix
cells are `max(1, historyRows-lags) × width`, which reserves an inference row even for warm-up calls. The ARIMA
state proxy is `max(p+s*P, q+s*Q+1) + d+s*D`, where `s` is the seasonal period. Automatic search checks its largest
orders and `(maxP+1)*(maxD+1)*(maxQ+1)+1` fits before it starts. These limits are fixed; there are no CRD fields for
them.

They are conservative proxies, not RSS or CPU-second limits or performance guarantees. Libraries can allocate
several arrays, optimizer workspaces or trees beyond the counted cells, so process deadlines, bounded concurrency and
container memory limits are still needed. If a model hits a budget, reduce its history, tree complexity or ARIMA
search and order.

## Sizing and limits

`RLIMIT_AS` bounds virtual mappings, not resident memory, total pod usage or a whole process tree. The pod's CPU
quota and memory limit give the aggregate limits, and the Go model deadline bounds each request's elapsed time. There
is no cumulative CPU-second limit on persistent processes, since it would eventually kill healthy long-lived
sessions. Models don't get their own cgroup, container or security boundary. Library threads are bounded, but a
virtual-memory ceiling doesn't guarantee numerical work or latency.

Size the pod for the Go operator, live histories, all retained workers, active one-shot fits, temporary copies and
headroom. The default 16-session cap doesn't mean sixteen simultaneous worst-case fits fit in the default 2Gi pod.
Reduce sessions or concurrency, or raise the pod resources, based on measured behavior. Limits that are too small can
stop numerical libraries from importing; that shows up as failed forecasts and the normal fallback and backoff.
