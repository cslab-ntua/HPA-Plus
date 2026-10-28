# Installation

Install the operator with Helm, then create `HPAPlus` resources for your workloads. No prebuilt image is published,
so build your own first.

1. Build and push the operator image to a registry your cluster can pull from:

   ```bash
   export REGISTRY=<your-registry-or-dockerhub-user>
   export VERSION=$(git rev-parse --short HEAD)

   make docker REGISTRY=${REGISTRY} VERSION=${VERSION}
   make push REGISTRY=${REGISTRY} VERSION=${VERSION}
   ```

2. Install the Helm chart from this repository with your image:

   ```bash
   helm upgrade --install hpa-plus-operator ./helm \
     --namespace hpa-plus-system \
     --create-namespace \
     --set image.repository=${REGISTRY}/hpa-plus-operator \
     --set image.tag=${VERSION}
   ```

The chart installs the CRD, the operator and its permissions. Then follow [getting started](getting-started.md) to
configure an existing workload.

## Operator deployment settings

The chart runs one replica with leader election enabled, and a namespaced Role grants the Lease permissions. More
than one replica requires `leaderElection: true`. In-memory Python fits are rebuilt after a leadership change, while
histories, cached forecasts and pending scale accounting live in ConfigMaps.

The default deployment strategy is `Recreate`, so two managers never run at the same time during an upgrade. The
operator pauses while the new pod starts; workload replicas keep running. `deploymentStrategy: RollingUpdate` is safe
when every manager uses the same leader-election lock.

The image is built for linux/amd64, and the default node selector enforces that architecture. An empty `image.tag`
falls back to the chart's app version.

Containers run as UID/GID 65532 with a read-only root filesystem, dropped capabilities, no privilege escalation and
the runtime's default seccomp profile. A writable `/tmp` emptyDir is capped at 128 MiB (`tmpSizeLimit`). Startup and
liveness probes use `/healthz`, and readiness uses `/readyz`; these checks don't certify forecast quality or metric
availability. CPU, memory and storage requests and limits are set with `resources`; the defaults are starting
budgets, not measured sizing. Numerical libraries use one thread per Python process.

Tree feature diagnostics are off by default. `featureDiagnostics.enabled` keeps XGBoost and LightGBM CSV snapshots
for each resource and model in ephemeral scratch storage: by default at most 256 rows and 1 MiB each, 64 files and
16 MiB in total, for one hour. Cleanup runs every minute, and diagnostic failures never fail a forecast. See
[feature diagnostics](../reference/feature-diagnostics.md).

## Admission and history

The chart uses the CRD schema for admission and installs no admission webhooks. The controller runs the cross-field
checks: a resource that fails them is stored but stays inactive, and the reason is logged and reported in its
`ConfigurationValid` condition. See [validation](../reference/configuration.md#validation).

History is bound to the workload's UID. Retargeting an HPAPlus or recreating its workload starts a new training
window and isolates incremental workers, and `predictionFailurePolicy` applies while the models warm up again. The
history ConfigMap must have a matching HPAPlus owner reference; an unrelated object with the expected name is not
adopted. Unreadable or unsupported history stops scaling until it is repaired. Keep a copy and read
[history recovery](../reference/history-recovery.md) before changing it: deleting it can lose the accounting for a
partly completed scale.

## Optional worker container

Set `workerIsolation.enabled=true` to run the Python models in a separate container with its own CPU and memory
limits and no Kubernetes service-account token. See [worker isolation](../reference/worker-isolation.md) for values,
recovery behavior and metrics endpoints.

## Optional network isolation

The chart supports `networkPolicy.enabled=true` with explicit ingress and egress rules. It is disabled by default.
Configure your API and DNS destinations before enabling it; empty rule lists create default-deny isolation. See
[network policy](../reference/network-policy.md) for configuration, preview commands, rollout checks and
network-plugin requirements. The policy applies to the whole operator pod, including an optional worker sidecar.

## Tuning hooks

HTTP tuning hooks are denied unless `hooks.allowedOrigins` lists their exact HTTP(S) origins. Redirects and URLs with
credentials are rejected, and models without hooks are unaffected. See [hooks](hooks.md) and
[hook security](../reference/hook-security.md).

For authenticated hooks, `hooks.credentials` binds existing Secrets in the operator namespace to explicit HPAPlus
namespaces, names and HTTPS origins. Only the manager mounts those tokens, and no Secret API permissions are added.
See [bearer authentication](../reference/hook-security.md#secret-based-bearer-authentication) for setup and rotation.

## Production operations

See [operating HPA+](operations.md) for capacity planning, warm-up, failover, backups and troubleshooting, and
[observability](observability.md) for operator status and metrics. Start new configurations in
[observe mode](rollout.md) with `spec.mode: observe`; an omitted mode means `control`. Install the image and the CRD
from the same checkout.
