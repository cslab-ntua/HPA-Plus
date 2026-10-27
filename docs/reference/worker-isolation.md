# Worker container isolation

Set `workerIsolation.enabled: true` to move **all Python execution** into a `model-workers` sidecar. This is opt-in;
existing single-container installations keep their behavior. Build and select an image containing `/app/worker-server`
before enabling the option; previously published images do not contain this binary.

```yaml
image:
  repository: your-registry/hpa-plus
  tag: your-tested-version
workerIsolation:
  enabled: true
  resources:
    requests:
      cpu: 250m
      memory: 256Mi
    limits:
      cpu: "2"
      memory: 2Gi
  tmpSizeLimit: 128Mi
# These remain per-process / pool limits inside the worker container.
workers:
  memoryMiB: 1024
  concurrency: 4
  sessions: 16
```

Manager resources remain under `resources`. Worker limits are additional, separately enforced container cgroup limits;
include both containers when sizing nodes and quotas. The schema requires positive CPU and memory limits. Memory
values use positive whole bytes or integer quantities such as `768Mi`/`2Gi`; CPU supports positive cores or millicores.
The per-process address-space ceiling remains distinct from resident memory and aggregate container memory.

## Boundaries and behavior

```mermaid
flowchart LR
    subgraph Pod[Operator pod]
        subgraph Manager[Manager container: own CPU and memory limits]
            Controller[Controllers and Go model adapters]
            Credentials[Kubernetes token and CA]
        end
        Socket[Private Unix socket on shared emptyDir]
        subgraph Workers[Worker container: own CPU and memory limits]
            Broker[Bounded execution service]
            OneShot[Bounded one-shot Python processes]
            Persistent[Bounded incremental ARIMA sessions]
            Broker --> OneShot
            Broker --> Persistent
        end
        Controller --> Socket --> Broker
    end
```

The manager uses `--worker-socket=/var/run/hpaplus/worker.sock`. A versioned HTTP/JSON protocol travels only over that
socket, with an allowlist of bundled model scripts, strict fields, bounded payloads/replies and bounded request admission.
Opaque session keys are SHA-256 digests, so the bounded wire identifier does not impose a new limit on public model names.
It propagates model deadlines and cancellation. Cleanup RPCs have a separate three-second bound and can add that
cleanup latency after execution is canceled. Reset/cleanup requests retry only explicit busy-admission rejections within their existing three-second deadline.
Execution is never automatically retried over the transport: an
interrupted incremental request may have advanced model state. Existing adapter reset/refit behavior owns recovery.
Unavailable workers return errors through the existing backoff/reactive-fallback policy; no Python is started in the
manager container. Valid cached predictions retain their normal expiry policy.

One shared worker container contains the model pool. A kernel OOM may kill a Python child or restart that container,
while the manager has its own cgroup. This does not assign a separate cgroup to each model, guarantee a particular OOM
victim, or isolate workers from each other. Both containers still share the pod network and host kernel. Network egress
policy, per-model containers and stronger tenant isolation are separate capabilities.

The pod disables automatic service-account token mounting. Only the manager receives a projected token, CA and
namespace volume. Workers have their own bounded scratch volume and no Kubernetes credentials. Both containers run as
UID/GID 65532 with a read-only root, dropped capabilities and RuntimeDefault seccomp. The socket is mode 0600; its
emptyDir is shared only by these two containers. This is a trusted bundled-code execution service, not a public API.

A new manager's first request clears resident sessions before starting work, preventing removed models from leaving
orphan sessions across manager restarts. Worker restarts lose in-memory sessions; adapters rebuild from persisted
history. Shutdown cancels in-flight calls and requests session cleanup. A lock protects the socket path, and startup
removes only a stale socket while holding that lock; regular files and symlinks are not replaced.

## Health and metrics

The sidecar's startup/readiness/liveness probes use HTTP `/healthz` on port 8082. Each request checks the actual private
Unix socket with a one-second deadline; Kubernetes allows two seconds for the HTTP probe. This avoids spawning a new
exec process for every probe under load. A failed or unresponsive socket returns a sanitized 503. Health checks bypass
execution admission, so a full model queue does not make the worker unhealthy. The manager remains independently healthy while
forecasts are unavailable; Kubernetes pod readiness also reflects sidecar readiness.

Scrape manager metrics on port 8080 and worker metrics on port 8082. Worker process/exchange activity, lifecycle and
queue metrics now live on the worker endpoint; forecast outcomes, decisions, history I/O and controller metrics remain
on the manager endpoint. Keep endpoint/container labels when aggregating process metrics. Port 8082 is read-only
metrics and socket health only; the execution API has no TCP listener. Unknown routes return 404.

When explicitly selecting an older worker image without the HTTP endpoint, set `workerIsolation.probeTransport: exec`.
The CLI `/app/worker-server --check` remains supported. Upgrading to the current image should use the default `http`;
when reusing Helm values from an older release, set this value explicitly. Helm rollback restores the previous probe configuration.
