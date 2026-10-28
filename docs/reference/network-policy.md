# Operator network isolation

The Helm chart can install a namespace-scoped `networking.k8s.io/v1` NetworkPolicy for its operator pods.
Set `networkPolicy.enabled: true` and supply explicit `ingress` and `egress` rules. It is disabled by default.
Enabling it with empty lists creates a policy with no allowed ingress or egress rules. Configure API access before enabling it on an operator.

## Configure destinations

Set `networkPolicy.ingress` and `networkPolicy.egress` to explicit rules for your cluster. Typical connections are:

| Connection | Allowance to configure | Purpose |
| --- | --- | --- |
| Kubernetes API | TCP 6443 to one explicit address | Watches, history, Scale/status writes, leader election and aggregated metrics APIs |
| Cluster DNS | UDP/TCP 53 to DNS pods in `kube-system` | Name resolution, including tuning hooks |
| Prometheus ingress | TCP 8080/8082 from selected monitoring pods | Manager metrics and optional isolated-worker metrics |
| Optional tuning hook | TCP 8443 to selected pods in `forecasting` | Holt-Winters runtime tuning; remove when unused |

Read the API Service and its endpoints to establish the addresses and ports in your environment:

```bash
kubectl -n default get service kubernetes -o yaml
kubectl -n default get endpoints kubernetes -o yaml
kubectl -n kube-system get pods --show-labels
```

Account for every API endpoint in an HA cluster, the API Service port and any service-to-endpoint translation.
Node-local DNS may require an explicit DNS IP rule instead of the pod selector. The operator accesses resource,
custom and external metrics through the Kubernetes API; it does not normally connect directly to metrics-server.
Unix-socket worker communication needs no network allowance. Hooks run in the manager and need their own egress rule.

The chart supports a deliberately restricted rule format:

- Each ingress rule requires nonempty `from` and `ports`; each egress rule requires nonempty `to` and `ports`.
- Peers use `ipBlock` (CIDR with optional nonempty `except`) or nonempty `matchLabels` selectors for namespaces,
  pods, or both. An IP block cannot be combined with selectors in one peer.
- Every port requires `protocol: TCP` or `UDP` and a numeric `port` from 1 through 65535.
- Empty peers/selectors, missing ports, unknown fields, named ports, port ranges and selector expressions are rejected.
  API admission validates CIDR syntax, exceptions and label syntax; Helm does not replace that validation.

An explicit broad CIDR is still broad access. Review rule scope rather than assuming that passing validation proves
least privilege. For selector expressions, named ports or other advanced features, manage a separate policy directly
and leave this chart feature disabled.

## Preview and rollout

Save your network settings as a local values file, render it, and validate against the intended API before installing:

```bash
helm template hpa-plus ./helm --namespace hpa-plus -f network-values.yaml \
  --show-only templates/cluster/network_policy.yaml
helm template hpa-plus ./helm --namespace hpa-plus -f network-values.yaml \
  --show-only templates/cluster/network_policy.yaml | kubectl apply --dry-run=server -f -
```

Use your existing release name, namespace and complete deployment values for the actual Helm upgrade. Verify API
watch/leader-election progress, fresh samples and forecasts, optional hook requests and metrics scraping after enabling.
Test allowed and denied connections on a disposable cluster using the same CNI and service routing as production.
If the operator loses API connectivity, disable `networkPolicy.enabled` through Helm to remove the chart-owned policy
or roll back to a previously verified revision. Keep other deployment values when doing so. A Helm rollback restores
the prior policy and its rules; this may restore restrictions as well as remove them.

## Enforcement boundaries

NetworkPolicy requires a supporting network plugin. Policies are additive: another policy can allow traffic that this
one omits. A namespace selector and pod selector in one peer are combined; separate peers are alternatives. A pod-only
selector refers to the policy namespace. Node traffic and host-network behavior have exceptions, and Service address
translation can change which IP/port the plugin evaluates. See the
[Kubernetes NetworkPolicy reference](https://kubernetes.io/docs/concepts/services-networking/network-policies/).

The manager and worker sidecar share one pod network namespace, so this policy cannot give them separate egress rights.
It also does not implement HTTP authentication, domain allowlists or per-request authorization. In particular it is not
a replacement for the [hook destination, redirect, metadata and Secret authentication controls](hook-security.md).
