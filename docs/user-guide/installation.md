# Installation

You can install HPA+ on your cluster after you have installed the HPA+ operator onto your cluster.

1. Build and push the controller image with the registry you plan to use:

   ```bash
   export REGISTRY=<your-registry-or-dockerhub-user>
   export VERSION=$(git rev-parse --short HEAD)

   make docker REGISTRY=${REGISTRY} VERSION=${VERSION}
   make push REGISTRY=${REGISTRY} VERSION=${VERSION}
   ```

2. Install the Helm chart from this repository while overriding the image fields:

   ```bash
   helm upgrade --install hpa-plus-operator ./helm \
     --namespace hpa-plus-system \
     --create-namespace \
     --set image.repository=${REGISTRY}/hpa-plus-operator \
     --set image.tag=${VERSION} \
     --set mode=cluster
   ```

After you have done that you can install `HPAPlus` objects onto your cluster. Follow the [getting started
guide](./getting-started.md) to configure an existing workload.

## Operator deployment settings

The chart defaults to one replica with leader election enabled. A namespaced Role grants lease and leader-election
event permissions. More than one replica requires `leaderElection: true`; full cluster failover qualification remains
outstanding. In-memory Python fits are recreated after leadership changes, while histories, optional cached predictions,
and pending scale accounting live in ConfigMaps.

The default deployment strategy is `Recreate`, so the first upgrade cannot overlap a legacy operator that ignored
leader election with a new leader. This introduces an operator pause during rollout; workload replicas remain running.
`deploymentStrategy: RollingUpdate` is available after every participating operator version uses the same leader lock.

The current image build is amd64, and the default node selector enforces that architecture. The chart and Makefile
share `0.14.0-hpaplus` release metadata; an empty `image.tag` uses the chart app version. Build and override the image
as shown above to run the current working tree. These changes do not publish a new registry image.

Containers run as UID/GID 65532 with a read-only root filesystem, dropped capabilities, no privilege escalation, and
the runtime's default seccomp profile. A writable `/tmp` emptyDir is capped at 128 MiB (`tmpSizeLimit`). Startup and
liveness probes use `/healthz`; readiness uses `/readyz`. These manager checks do not certify forecast quality or metric
availability. CPU/memory/storage requests and limits are configurable through `resources`; defaults are initial budgets,
not benchmark-derived sizing guarantees. Numerical library thread counts are capped at one per Python process.

Tree feature diagnostics are disabled by default. Enable `featureDiagnostics.enabled` to retain separate XGBoost and
LightGBM CSV snapshots for each resource/model identity in ephemeral scratch storage. Defaults cap snapshots at 256 rows
and 1 MiB each, with 64 files/16 MiB total and one-hour retention. Idle cleanup runs every minute; diagnostic failures
never fail a forecast. The old `XGB_FEATURE_DUMP`/`LGBM_FEATURE_DUMP` shared paths are no longer honored. See
[feature diagnostics](../reference/feature-diagnostics.md) for configuration, retrieval, limits and migration.

## Admission and upgrades

The chart uses the CRD schema for API-server admission and does not install admission webhooks. Additional cross-field
checks run in the controller; resources that fail those checks can be admitted but are logged and left inactive.

Earlier charts included nonfunctional `mutating-webhook-configuration` and `validating-webhook-configuration` objects
pointing to `webhook-service` in namespace `system`. Upgrade the existing Helm release with the command above (using its
original release name and namespace). Helm removes those objects when they were managed by that release. Do not roll
back to an affected chart, since it reinstalls the broken webhooks.

If admission still reports a missing `webhook-service`, inspect the two webhook objects and their Helm ownership
annotations. Objects installed manually or owned by another release are not removed by upgrading this release; resolve
their ownership before removing obsolete HPA+ webhook registrations.

The LightGBM schema permits `maxDepth: -1` for unbounded depth; positive integers also work. Zero is rejected by the
controller. XGBoost requires positive depth when provided. Omitted depth uses the algorithm's default.

History is now bound to the resolved workload UID. Existing ConfigMaps without that identity start a fresh training
window once; the HPA baseline is used while models warm up. Retargeting HPAPlus or recreating its workload also resets
training/cached predictions and isolates incremental workers. Once status is identity-bound, old scale-policy windows
are cleared on replacement; initial adoption preserves legacy rate accounting conservatively.

### Persisted history compatibility

History ConfigMaps now carry `schemaVersion: 1`. Unversioned data migrates on its next successful write, preserving any
pending scale journal. The ConfigMap must have a matching HPAPlus owner reference; an unrelated object with the expected
name is not adopted. Unsupported versions and unreadable JSON leave the payload untouched and prevent scaling until
corrected. Preserve a copy and investigate the pending journal before repairing such data; deleting it can lose rate
accounting for a partially completed scale operation.

Training-window corruption is handled separately: the affected model warms up again while healthy models and the HPA
baseline remain available. Oversized ConfigMap writes are rejected before the API call. Reduce retained histories or
model count to recover space; automatic storage eviction is not implemented.

## Optional worker container

A locally built release image can run Python models in a separate container by setting `workerIsolation.enabled=true`.
The worker has independent CPU/memory limits and receives no Kubernetes service-account token. Select an image built
from this source before enabling it; older published images do not include the worker service. See
[worker isolation](../reference/worker-isolation.md) for values, recovery behavior and metrics endpoints.

## Optional network isolation

The chart supports `networkPolicy.enabled=true` with explicit ingress/egress rules. It is disabled by default.
Configure your API and DNS destinations before enabling it; empty rule lists create default-deny isolation.
See [network policy](../reference/network-policy.md) for configuration, preview commands, rollout checks and
network-plugin requirements. The policy applies to the whole operator pod, including an optional worker sidecar.

## Tuning-hook upgrade requirement

New source-built versions deny HTTP tuning hooks unless `hooks.allowedOrigins` lists their exact HTTP(S) origins.
Existing hook users must configure this value when upgrading. Redirects and URL-embedded credentials are rejected;
models without hooks are unaffected. See [hook security](../reference/hook-security.md) for flags, configuration and authentication.

For authenticated hooks, `hooks.credentials` binds existing Secrets in the operator namespace to explicit HPAPlus
namespaces/names and HTTPS origins. Only the manager mounts those tokens; no Secret API permissions are added.
See the [authentication guide](../reference/hook-security.md#secret-based-bearer-authentication) for rotation and migration.

## Production operations

See [operating HPA+](operations.md) for capacity planning, warm-up/deadlines, failover, backups, upgrade compatibility
and troubleshooting. The [observability guide](observability.md) documents operator status and aggregate metrics.

For new predictive trials, use explicit [observe mode](rollout.md). Install the matching mode-aware image and CRD
first; omitted/null mode preserves legacy control. The [API compatibility guide](../reference/api-compatibility.md)
explains defaults and why old binaries must not be used while relying on rollout-mode safety.
