# HTTP tuning-hook destination controls

Tuning hooks can only reach origins on an allowlist that the operator administrator sets. An empty list, the
default, disables hook requests. A denied hook fails that model attempt through the normal failure, backoff and
fallback path; it never continues with unapproved tuning values. Models without a hook are unaffected. The
[hooks guide](../user-guide/hooks.md) shows how a hook is configured on a model.

## Configuration

Configure the Helm values:

```yaml
hooks:
  allowedOrigins:
    - https://tuning.example.com
    - http://tuning-hook.forecasting.svc:8443
```

The corresponding manager flag is
`--hook-allowed-origins=https://tuning.example.com,http://tuning-hook.forecasting.svc:8443`.
No spaces or wildcards are accepted. Each entry is an HTTP(S) origin: scheme, hostname and optional port, with an
optional trailing slash. Paths, queries, fragments and URL credentials are rejected. Hostnames are case-insensitive;
omitted HTTP/HTTPS ports mean 80/443. Subdomains and different ports/schemes need separate entries. IPv6 literals
use brackets. The policy accepts at most 64 entries, each at most 300 bytes. Invalid flag configuration stops startup
before connecting to Kubernetes; Helm checks the value shape and the binary checks full origin syntax.

The allowlist is configured by the operator administrator, not by individual HPAPlus objects. It is shared by all
resources managed by that operator. Every resource author can request any path on an allowed origin, so only approve
dedicated, trusted tuning services. Plain HTTP is supported for cluster services but provides no TLS confidentiality
or endpoint authentication. HTTPS uses normal CA and hostname verification; there is no skip-verification option.
This flag belongs only to the manager; the worker sidecar does not make tuning requests.

## Request boundary

All 3xx responses are rejected, including same-origin redirects and redirects listed as successful status codes.
Configure the final endpoint directly. Requests do not use environment HTTP proxies. A dedicated transport resolves
the approved host, validates every returned address, then connects to a validated numeric IP without a second DNS
lookup. Every new connection repeats the checks; an existing connection may be reused. The original HTTP Host and
TLS server name remain intact. A mixed permitted/blocked DNS answer is rejected before any connection.

The address checks block loopback, unspecified, multicast and link-local addresses, IPv4 zero-network addresses,
known metadata/platform endpoints and selected IPv4 translation/transition ranges. IPv4-mapped IPv6 addresses are
checked as IPv4. Explicit metadata/platform entries include `100.100.100.200`, `168.63.129.16`, `fd00:ec2::254` and
`fd20:ce::254`; link-local checks cover `169.254.169.254`. The provider references are
[Alibaba metadata](https://www.alibabacloud.com/help/en/ecs/user-guide/view-instance-metadata/),
[Azure platform address](https://learn.microsoft.com/en-us/azure/virtual-network/what-is-ip-address-168-63-129-16),
[AWS metadata](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/configuring-instance-metadata-service.html) and
[Google metadata](https://docs.cloud.google.com/compute/docs/metadata/overview).

Private RFC1918/ULA addresses remain available for approved cluster services. This is not a complete prohibition on
access to private networks, nor a guarantee about every cloud provider or custom routing/NAT configuration. Trust in
the approved hostname and its DNS administration still matters. Combine the allowlist with explicit
[NetworkPolicy destinations](network-policy.md), dedicated hook services and infrastructure metadata protections.

Hook request failures expose fixed error text or an HTTP status number. They do not include the request URL/query,
upstream response body or raw transport error. Cancellation/deadline errors remain identifiable by callers. The
64-KiB response-body limit and the parent, model and hook timeouts still apply; response headers have a 32-KiB
transport limit.
See the Go [HTTP transport documentation](https://pkg.go.dev/net/http#Transport) for the underlying connection model.

## Secret-based bearer authentication

The operator can mount existing Secrets and bind bearer tokens to exact HPAPlus namespace/name and HTTPS-origin
combinations. No credential reference or token is added to the HPAPlus API. A resource author cannot select arbitrary
Secrets or broaden a binding. Configure the corresponding Helm values:

```yaml
hooks:
  allowedOrigins: [https://tuning.example.com]
  credentials:
    - name: checkout-tuning
      origin: https://tuning.example.com
      namespace: workloads
      resourceNames: [checkout]
      secretName: checkout-tuning-token
      secretKey: token
```

Create/manage `checkout-tuning-token` separately in the **operator release namespace**, with the token under key
`token`. `namespace: workloads` selects the HPAPlus namespace; it is not a Secret namespace. Do not put token values
in Helm values or HPAPlus headers. Use your normal Secret provisioning process; the chart references existing Secrets
and neither creates them nor deletes them when a binding is removed. Operator RBAC gains no Secret get/list/watch
permission. A missing required Secret/key prevents kubelet from mounting the pod's projected volume.

The chart mounts a read-only projected volume at `/var/run/hpaplus-hook-auth` in the manager only, with mode `0440`
and the existing pod `fsGroup`. A non-secret ConfigMap supplies `bindings.json`; selected Secret keys supply
`tokens/<binding-name>`. The manager flag is `--hook-credentials-dir=/var/run/hpaplus-hook-auth`. The isolated worker
container receives neither this volume nor this flag. In direct execution mode, Python shares the manager container
filesystem; use worker isolation when that separation is needed.

Bindings require HTTPS and must also match the origin allowlist. The controller supplies the HPAPlus identity from
its instance to the hook via context. A matching binding adds `Authorization: Bearer <token>`; a different resource,
namespace or origin receives no managed token. Requests with missing identity fail when credentials are configured.
Matching requests with missing, oversized, empty or malformed tokens fail before sending HTTP. Inline Authorization
headers conflict with managed authentication and are rejected, including empty or differently cased headers.
Other inline headers are allowed, but they are stored in the HPAPlus object, so keep credentials out of them.

The token file is bounded to 8 KiB. Leading/trailing whitespace is trimmed; the remainder must use the bearer-token
character grammar from [RFC 6750](https://www.rfc-editor.org/rfc/rfc6750#section-2.1). Tokens are read anew for every
matching request and are never cached in operator state. Projected Secret updates are eventually propagated by kubelet;
this chart uses no `subPath` mount, so subsequent requests use the new file after projection updates. See the
[Kubernetes Secret volume documentation](https://kubernetes.io/docs/concepts/configuration/secret/#using-secrets-as-files-from-a-pod).
An already-issued request may finish using its old token. A deleted Secret may leave previously projected data until
the platform reconciles it; revoke old credentials at the hook service when immediate revocation is required.

Bindings are loaded once at startup (bounded to 1 MiB, 64 bindings and 64 resource names per binding). Duplicate
names or overlapping resource/origin grants are rejected. Helm binding edits change a pod-template checksum and
roll the manager; Secret token rotation alone does not. Bindings use namespace/name, so recreating an HPAPlus under
the same name inherits that grant. Every path on the approved origin is eligible: dedicate and trust that service,
and restrict who can edit an authorized HPAPlus. This does not add OAuth token refresh, basic auth or mutual TLS.

The tuning request includes a copy of the model **without its hook configuration**. Hook URLs, inline headers and
authentication details are absent from both body-mode and query-mode payloads; the configured model is unchanged.
Filesystem and authentication errors have fixed text without token content or filesystem paths. Redirects are
disabled, response errors are sanitized and URL-embedded credentials are rejected.

## Enabling a hook

Add the hook's exact final origin to `hooks.allowedOrigins`, use a URL without redirects or embedded credentials, and
allow the egress in your network policy. Then check the model's status for successful fits. Without the flag the
manager denies every hook, and the chart leaves the flag out when the list is empty.
