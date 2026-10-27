# Operator permission contract

The generated
ClusterRole follows markers in `internal/controllers/controller.go`; leader-election permissions live in the separate
namespaced chart Role.

| Resource | Retained verbs | Reason |
| --- | --- | --- |
| `hpa.plus/hpapluses` | get, list, watch | Reconciliation, independent sampling and current-identity checks |
| `hpapluses/status` | patch | Optimistic, owned-field status updates |
| `hpapluses/finalizers` | update | Admission authorization for legacy blocking history owner references |
| Core ConfigMaps | get, list, watch, create, update, delete | History roots/chunks, cached reads, publication and cleanup |
| Core Pods | get, list | Workload metrics evaluation and readiness accounting |
| Deployments, ReplicaSets, StatefulSets, ReplicationControllers | get, list, watch | Target resolution, CPU requests and cached workload reads |
| Supported workload `/scale` | get, update | Read current replicas and conditionally update desired replicas |
| `metrics.k8s.io/pods` | get, list | Resource metrics; node metrics are unused |
| Custom/external metric resources (`*`) | get, list | Metric names and custom object kinds are dynamic |
| Election Lease `a07591f8.hpa.plus` in release namespace | get, update | Election observation and renewal |
| Leases in release namespace | create | Election bootstrap |
| Core Events across workload/release namespaces | create, patch | Condition/scale Events and leader-election Events |

Removed grants include HPAPlus creation/deletion/spec updates, status get/update, Scale patch, ConfigMap patch,
node resource metrics and arbitrary Lease get/update/patch. Workload mutation outside `/scale`, Secret API access,
pod exec/log access, service-account token creation and RBAC administration are not granted.

The operator normally creates nonblocking history owner references. Existing roots can nevertheless contain a
blocking reference that is copied to new history chunks. Kubernetes authorizes such references through the owner's
`finalizers` permission, so that narrowly scoped grant is retained. See the upstream
[owner-reference admission implementation](https://github.com/kubernetes/kubernetes/blob/v1.26.1/plugin/pkg/admission/gc/gc_admission.go).
This authorization does not grant HPAPlus spec updates.

## Remaining scope

The chart grants **cluster-wide permissions**. The manager supports `--watch-namespace` to restrict watched resources,
but that runtime setting does not narrow the service account's RBAC permissions. History names and chunk names are dynamic, so the role can mutate unrelated
ConfigMaps as well as history objects; runtime ownership checks are not an RBAC boundary. Likewise it can update Scale
on all supported workloads. Custom/external metrics keep read-only resource wildcards because provider-specific
names cannot be predicted by the chart. These permissions are explicitly retained, not claimed to be tenant isolation.
A namespace-restricted permission boundary requires separately scoped roles and bindings.

Lease creation cannot be constrained with `resourceNames` using ordinary RBAC, as described in the
[Kubernetes RBAC reference](https://kubernetes.io/docs/reference/access-authn-authz/rbac/#referring-to-resources).
Bootstrap therefore retains create
within the release namespace, while get/update are limited to the fixed lock name. Disabling leader election removes
the Lease Role and binding. Core Event create/patch stays in the ClusterRole because condition/scale reporting is
independent of election. Other cluster-installed bindings can grant additional permissions.

Hook credentials use manager-only projected Secrets and add no Secret API rights. Kubelet mounting is separate from
operator API authorization; see [hook authentication](hook-security.md#secret-based-bearer-authentication).
