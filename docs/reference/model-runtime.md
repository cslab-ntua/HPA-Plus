# Model runtime and retry controls

The operator runs bounded one-shot Python processes and retained ARIMA workers. Both enter
`algorithms/worker_limits.py` before loading numerical libraries. The wrapper sets the soft and hard Linux
`RLIMIT_AS` limits together, preserving any stricter inherited hard ceiling. An allocation beyond the ceiling fails
inside that worker. A failed exchange evicts/reaps its persistent worker; later attempts can start a clean process.
One-shot failure also releases its concurrency slot. Existing output/input limits, process-group cancellation,
reaping, per-model wall deadlines and deterministic training/search ceilings remain active.

Persistent reset keeps its admission slot until the old child has been reaped. Admission rechecks cancellation after
waiting for the session registry, so a canceled waiter cannot start a replacement child. The Unix client can return
its deadline before server-side cancellation finishes; server-side cleanup may still be completing. A client timeout alone is not evidence that server cleanup has completed.

| Helm value | Binary flag | Default | Range/meaning |
| --- | --- | --- | --- |
| `workers.memoryMiB` | `--worker-memory-mib` | 1024 | Hard virtual address space per process; 64..65536 MiB, or 0 to disable. |
| `workers.concurrency` | `--worker-concurrency` | 4 | 1..64 simultaneous one-shot processes; queued calls share their model deadline. |
| `workers.sessions` | `--worker-sessions` | 16 | 1..64 retained incremental processes. An idle session still holds its model. |
| `modelRetry.initial` | `--model-retry-initial` | `0s` | 0 follows syncPeriod; otherwise at least 1s and at most the ceiling. |
| `modelRetry.maximum` | `--model-retry-max` | `5m` | 1s..24h; zero selects the default five minutes. |

The binary rejects invalid limits at startup. Helm also validates numeric ranges and duration syntax; cross-field
and exact duration bounds are checked by the binary. Numerical-library thread settings are forced to one in both
runners, including BLAS, OpenMP, MKL, BLIS and NumExpr, regardless of inherited host settings. The supported deployment
is Linux. The wrapper is part of the matching source/image build; do not pair a new binary with old algorithm files.

## Retry circuit behavior

Each failed fit opens that model's circuit immediately. The next eligible reconciliation at or after RetryAfter is
the next probe; it can run later because model cadence and reconciliation scheduling still apply. Failed probes
double the delay up to the ceiling. A successful fit clears the circuit. Failures are fingerprint-scoped and persist
through controller restarts. Warmup and parent cancellation do not increment them; capacity/spec/identity changes and
window resets invalidate old state. Sampling and other models continue. This does not disable a model permanently.

Policies are immutable for a running operator and apply across its resources. A policy change is picked up on restart.
A previously stored retry remains valid within the new maximum, even if the initial delay changed; new failures use
the new policy. A stored delay exceeding the new ceiling is treated as invalid and permits a fresh attempt. All leaders
should use the same configuration. Retry timestamps round up to whole seconds so serialization cannot shorten the
wait. Incremental failure can still attempt one-shot fallback within the original model deadline; only a final failed
model result advances its circuit.

## Sizing and limits

RLIMIT_AS bounds virtual mappings, not resident memory, aggregate pod usage or an entire descendant tree. The pod's
CPU quota and memory limit provide aggregate limits; the Go model deadline bounds each request's elapsed duration.
There is no cumulative CPU-second limit on persistent processes, which would eventually kill healthy long-lived
sessions. There is no new per-model cgroup, container, or security boundary. Threads from supported libraries are
bounded, but a virtual-memory ceiling is not a numerical work/latency guarantee.

Size the pod for the Go operator, live histories, all retained workers, active one-shot fits, temporary copies and
headroom. The default 16-session cap is not a claim that sixteen simultaneous worst-case fits fit in the default 2Gi
pod. Reduce sessions/concurrency or raise pod resources using measured workload behavior. Limits that are too small
can prevent numerical libraries from importing; they produce failed forecasts and the normal fallback/backoff path.
