# Operating HPA+

Use HPAPlus status to inspect a specific workload and the operator's logs and aggregate metrics to inspect controller health. The [observability guide](observability.md) documents the metrics and status interpretation.

## Resource sizing

Budget for the Go manager, retained histories, incremental sessions and concurrent Python fits. `workers.memoryMiB` limits each process's virtual address space; `resources` and optional `workerIsolation.resources` bound aggregate container usage. A session remains resident between forecasts. Reduce `workers.sessions` or `workers.concurrency` if resource pressure persists, or increase resources based on observed usage.

The worker queue and execution share the model deadline. Increasing fit complexity without sufficient CPU can cause timeouts or forecasts that finish after expiry. A successful fit is not sufficient for eligibility: the controller also checks history, source identity, capacity, configuration and timing. See [runtime limits](../reference/model-runtime.md).

## Warm-up, missing CPU and fallback

Inspect `status.modelStatuses`, conditions and current metrics. Models need enough complete CPU observations; missing measurements do not count as zero demand. Pod starts, termination and incomplete container coverage can delay usable deployment measurements.

Baseline-only decisions can be expected during warm-up, failed fits, expired predictions or unavailable history. Determine which condition applies before changing model parameters. Check prediction horizon and age separately from fitting duration and collection age. Existing valid cached forecasts keep their configured eligibility; retries do not extend their source timestamps.

The optional `cpuHistorySource: kubelet` path changes the CPU training source and requires node-proxy access. It does not guarantee complete or earlier readings. Reactive evaluation continues through the normal metric path.

## Status, history and storage

History is tied to the HPAPlus and target UIDs. Resource recreation or retargeting starts a new window rather than merging different workload histories. Histories use a root ConfigMap and, when needed, immutable chunks. Preserve the root and all referenced chunks when capturing state; a root-only copy is insufficient for allocated history.

Inspect `status.historyCapacity` for estimates and actual storage errors. Resource quotas must permit the committed snapshot plus staged replacements. The controller rejects unsafe writes instead of silently evicting models or truncating their configured windows. Status has a separate byte budget that preserves scaling accounting.

For corrupt data, preserve the HPAPlus, target state, root and referenced chunks before intervention. Investigate pending Scale intent before modifying history; deleting a journal can discard accounting for a write that already occurred. See [history storage](../reference/history-storage.md) and [recovery](../reference/history-recovery.md).

## Leadership and upgrades

Keep leader election enabled when running multiple managers, and use the same lock configuration and runtime limits. Worker sessions are transient and rebuilt from stored history after leadership or container changes. The default Recreate rollout avoids overlap with older managers that did not honor the same election policy; it introduces a pause in operator processing while application pods remain running.

Install matching binaries, algorithm files and CRD definitions. Review explicit defaults and history compatibility before downgrading. In particular, older versions may not understand observe mode or allocated history. Do not rely on mode safety with a binary that ignores that mode. See [API compatibility](../reference/api-compatibility.md).

## Inspection commands

Replace names and namespaces with the actual installation and workload:

```bash
kubectl -n application-namespace get hpaplus application -o yaml
kubectl -n application-namespace get deployment application -o yaml
kubectl -n application-namespace get pods
kubectl -n hpa-plus-system logs deployment/hpa-plus-operator -c hpa-plus-operator --since=10m
```

A requested replica count, an accepted Scale write, an observed pod count and ready replicas are different stages. Use [decision reporting](../reference/decision-reporting.md) to distinguish them. Enable `logging.level: debug` for correlated model detail when investigating; feature snapshots are separately opt-in and bounded.

Scrape the manager on port 8080 and an isolated worker on port 8082 using your existing monitoring system. Preserve endpoint labels when combining worker and manager metrics. NetworkPolicy rules must permit the Kubernetes API, DNS, configured hooks and monitoring traffic needed by your installation.
