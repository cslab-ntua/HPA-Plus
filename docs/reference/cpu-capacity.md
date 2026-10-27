# CPU capacity and pod resource policy

HPA+ forecasts aggregate serving-container CPU demand. Its capacity denominator is a declared CPU request, not a
measurement of throughput or a guarantee of available node CPU. This policy applies to both baseline resource
utilization and predictive conversion; no new public configuration fields are required.

## Resolving one pod or template

| Shape | Request used |
| --- | --- |
| Regular containers, including traditional sidecars | Sum of their positive requests |
| Native sidecars (`initContainers[].restartPolicy: Always`) | Added to regular container requests |
| Classic init containers | Excluded from steady-state container sums |
| Explicit `spec.resources.requests.cpu` | Overrides container CPU requests for the whole pod |
| Pod resources declaring only another resource, or only limits | Fall back to the container sum for CPU |
| Runtime `spec.overhead` | Excluded from application capacity |
| Missing/nonpositive request, unknown init restart policy, quantity or sum overflow | Capacity unavailable |

The same per-resource rules apply to baseline memory utilization. A Pod-level request permits containers without
individual requests for that resource. Every serving container needs a positive request when that Pod-level override
is absent. Quantities round upward to milli-units, with range checks **before** conversion and addition.

These are HPA+'s steady-state rules, not scheduler effective-request accounting. In particular, init peaks and runtime
overhead are not added to CPU demand capacity. Kubernetes 1.34's HPA also includes restartable init containers; its
Pod-level path uses Kubernetes resource helpers, whose overhead/init accounting differs from this explicit HPA+
policy. See the [versioned upstream HPA implementation](https://github.com/kubernetes/kubernetes/blob/v1.34.0/pkg/controller/podautoscaler/replica_calculator.go)
and [sidecar resource behavior](https://kubernetes.io/docs/concepts/workloads/pods/sidecar-containers/#resource-sharing-within-containers).
Do not interpret HPA+ results as exact parity with every Kubernetes HPA feature-gate combination.

The pinned Go Kubernetes types predate the new fields. The collector therefore reads raw Pod JSON using the dynamic
client, and template lookup uses an unstructured API read. Both call the shared request resolver. The legacy evaluator
receives a private typed projection containing one request carrier; metadata and readiness remain intact. That
projection is never written to Kubernetes. One raw Pod list is shared by metric specs within a gather, with the same
namespace, selector, cancellation, rate budget and 30-second collection deadline. Validation is scoped to each
metric, so unavailable memory requests do not discard valid CPU observations or independent external metrics.

## Mixed requests, rollouts and VPA

Predictive request per pod is the **minimum positive request among listed active pods**, further reduced if a valid
native workload template declares a smaller request. A larger template cannot inflate capacity while smaller old
pods remain. The previous template-first / observed-mean fallback is intentionally replaced. For example, live pods
requesting 100m and 400m with a new 500m template use 100m; with a new 50m template they use 50m. Once all live pods and
the template request 500m, conversion uses 500m.

Template lookup supports `apps/v1` Deployment, ReplicaSet and StatefulSet and `v1` ReplicationController. API version
and kind must both match. Unsupported targets, unavailable templates or invalid template requests use observed
minimum capacity. Debug logs report `observed-minimum`, `workload-template-minimum` or `unavailable`. Capacity remains
part of the forecast-cache fingerprint: changes invalidate cached replica candidates. Training history continues to
store raw CPU demand independently of capacity.

This deliberately favors extra replicas during mixed rollouts over assuming that the pods left after downscaling
will have average capacity. It can overprovision and is still an estimate: admission-time VPA changes to future pods,
CPU throttling, node contention and application throughput are not predicted. Running horizontal and vertical
controllers on the same CPU signal still requires coordinated operator policy. No automatic VPA tuning is provided.

During a known in-place resize, utilization for that resource is unavailable. Both legacy nonempty `status.resize`
and true `PodResizePending` / `PodResizeInProgress` conditions trigger this guard. If actual or allocated request
status is present, it must equal the desired request for the selected denominator. Container status is checked when requests are summed;
a Pod-level override checks Pod-level status instead, allowing containers to inherit its budget. Absent status fields remain compatible with older servers. Settled requests restore normal collection.
Resource `AverageValue` and independent metric families do not require a utilization denominator.

## Partial observations and decisions

The collector excludes deleting, failed and succeeded pods and their metrics. It checks resource-metric container
names against all regular and native-sidecar containers. Missing, duplicate or unexpected container samples make
that pod's aggregate unavailable rather than recording understated CPU demand. Readiness and missing-pod handling
then run through the baseline evaluator.

A partial CPU population—missing/ignored pods, mismatched pod counts or unmatched request/usage records—is **not a
training sample**. It also enables the planner's incomplete-metrics guard, preventing baseline or model candidates
from downscaling below the currently requested replicas. Valid upscale candidates and explicit replica bounds still
apply. Complete observations restore normal training/downscale behavior. No zeros or utilization-derived values are
substituted into raw CPU history. Existing persisted windows are retained: older rows do not contain enough pod
coverage metadata to retroactively prove completeness. If earlier telemetry was incomplete, use observe mode while
fresh complete samples refill the training window.

Collection and template/Scale reads are not an atomic Kubernetes transaction. The existing observation cache can
reuse a snapshot for at most one second (or the shorter sync period), and final Scale conflict checks still apply.
These guards describe observed state; they cannot guarantee that no pod changes between collection and a Scale write.
