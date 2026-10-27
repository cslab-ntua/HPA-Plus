# HPA+ architecture

HPA+ retains the Kubernetes HPA configuration shape while adding CPU-demand forecasts. Reconciliation computes recommendations and applies scaling; a separate sampler records observations. Both use the same ownership, metrics, history and validation contracts.

## Components

| Path | Responsibility |
| --- | --- |
| `api/v1alpha1/` | HPAPlus configuration, status, defaults and generated deepcopy methods |
| `internal/controllers/` | Reconciliation, sampling, history publication, Scale writes and recovery |
| `internal/metrics/` | Kubernetes metrics, pod CPU completeness, source identity and capacity |
| `internal/decision/` | Candidate selection, rollout modes, constraints and accounting |
| `internal/history/` | Strict persisted history representation and storage interfaces |
| `internal/prediction/` | Typed model adapters, forecast timing and CPU-to-replica conversion |
| `internal/algorithm/`, `algorithms/` | Bounded Go/Python execution, model implementations and protocol |
| `internal/worker/`, `cmd/worker-server/` | Optional isolated model execution over a private Unix socket |
| `internal/hook/` | Administrator-controlled runtime tuning hooks |
| `internal/observability/` | Aggregate telemetry, structured logs and bounded Events |
| `helm/` | Operator deployment, CRD, resources, RBAC and optional isolation |

## Reconciliation and sampling

A reconciliation validates the resource, resolves its target, checks workload identity and recovers pending scale accounting. It gathers metrics, computes the reactive recommendation, loads retained history and evaluates models whose history, cadence and retry state permit execution.

Models return CPU demand rather than deriving demand from old replica counts. The runtime validates source identity and units, forecast provenance and validity. It converts eligible results to replicas using the workload's conservative CPU request capacity and utilization target. `includeHPA` controls whether the reactive recommendation joins available predictive candidates; when no prediction is available, the controller retains its fallback behavior.

The decision planner combines candidates using `decisionType`, then applies rollout mode, incomplete-measurement protection, bounds, stabilization and rate policies. `observe` publishes a counterfactual result without a new Scale write. `assist` protects against predictive downscaling below the reactive baseline. `control` applies the configured candidate policy.

The independent sampler checks configured sampling slots and records only complete usable deployment CPU observations. It preserves source timestamps, rejects duplicate source observations and isolates histories by workload UID. Missing measurements do not become zero-valued training rows. The default `syncPeriod` is 15,000 milliseconds; resources can set another cadence.

See [configuration](reference/configuration.md), [CPU capacity](reference/cpu-capacity.md) and [rollout modes](user-guide/rollout.md).

## Forecasting boundary

Each model adapter implements the shared typed forecasting interface. Python receives validated timestamped CPU history and model configuration; its output carries demand, timing and provenance. The model suite includes Linear, Holt-Winters, one-shot or incremental ARIMA/SARIMA, XGBoost and LightGBM.

Scalar policies convert a forecast to a replica candidate. Optional timed policies consume a trajectory, preserve a still-valid forecast covering the current interval and account for configured lead time before future increases. Future decreases cannot replace the current interval prematurely. Optional reliability policies compare eligible forecast outcomes using actual measurements; missing outcomes remain unavailable.

History length, model cadence, fit deadlines, prediction age and forecast horizon are distinct controls. A successful fit can be excluded if its result expires before use. Missing or unevenly spaced CPU measurements affect the information supplied to the models; the runtime cannot reconstruct unobserved CPU demand.

See [models](user-guide/models.md), [typed forecasting](reference/typed-forecasting.md), [forecast policy](reference/forecast-policy.md) and [worker protocol](reference/worker-protocol.md).

## History and scale accounting

History belongs to an HPAPlus UID and resolved workload UID. Retargeting or recreating the workload invalidates its old history and forecasts. Sampling, forecast results and scale accounting use conditional updates and explicit field ownership so concurrent changes do not overwrite newer state.

Small histories use a root ConfigMap. Larger histories allocate immutable chunks referenced by a versioned root manifest. Readers validate ownership, identities, sizes and hashes. Publication stages complete replacement data before conditionally publishing its manifest. The backend does not silently shorten configured training windows to fit.

Before changing Scale, the controller persists a journal of the intended operation. A later recovery resolves accounting without blindly replaying an old scale request. Kubernetes does not provide a transaction spanning specification, history, Scale and status; identity, resource-version and freshness checks reduce that race surface.

See [history storage](reference/history-storage.md) and [corruption and recovery](reference/history-recovery.md).

## Runtime and deployment

The manager runs in a Linux/amd64 image with the pinned Python algorithms. One-shot workers have bounded concurrency, incremental workers have bounded retained sessions, and model calls have deadlines and cancellation. Failed fits use persisted, fingerprint-scoped retry backoff. Numerical library threads are bounded; per-process address-space limits remain distinct from aggregate container memory limits.

With worker isolation enabled, the manager sends all model requests to a sidecar through a private Unix socket. The worker has separate resources and scratch storage and no Kubernetes service-account token. Unavailable workers produce the normal failure/cache/fallback behavior; the manager does not silently execute Python locally.

The Helm chart enables leader election and defaults to a Recreate rollout strategy. The manager can be restricted with `--watch-namespace`; that limits watched workloads, while the permissions granted to its service account remain a separate deployment concern. The optional kubelet history source requires additional node-proxy access; the default CPU source remains Metrics Server.

See [installation](user-guide/installation.md), [worker isolation](reference/worker-isolation.md), [runtime limits](reference/model-runtime.md), [RBAC](reference/rbac.md), [network policy](reference/network-policy.md) and [hook security](reference/hook-security.md).

## Operational visibility

HPAPlus status reports metric availability, model warm-up and eligibility, failure/backoff state, capacity and the latest decision. JSON logs distinguish candidate selection, scaling constraints, API write outcomes and accounting. Aggregate metrics are available on the manager endpoint and, when isolated, the worker endpoint.

These observations describe different stages: a forecast can succeed but expire, a Scale write can be accepted before pods become ready, and sampling can be active while some measurements are incomplete. See [observability](user-guide/observability.md), [decision reporting](reference/decision-reporting.md) and [operations](user-guide/operations.md).
