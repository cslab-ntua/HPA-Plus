# Benchmark results

HPA+ was tested on a live Kubernetes cluster against the Kubernetes HPA formula, each scaling a copy of the same
application under the same traffic. Each kind of traffic has its own page:

- [Synthetic traffic](synthetic.md): a 4-hour run with repeated peaks, slow cycles and a sudden jump, plus shorter runs
  on other shapes. The demand of every moment is known exactly.
- [Real traffic: 1998 World Cup](world-cup.md): one day of the World Cup web site's requests, replayed 8 times faster,
  with three forecasting models.

## What was compared

Copies of the same application received the same requests at the same times. Each copy was scaled by its own
`HPAPlus` resource.

| Setup | How it decides the pod count |
| --- | --- |
| **HPA+** | [Request demand](../user-guide/request-demand.md): it rebuilds the CPU demand of each 30-second window from measured CPU and the requests that arrived and completed, so a saturated pod no longer hides the work it could not serve. A model forecasts that demand 90 seconds ahead, and HPA+ starts pods early enough to be ready when a forecast rise arrives. It never recommends fewer pods than the current demand needs. |
| **HPA formula** | HPA+ with no models and `includeHPA: true`: the Kubernetes HPA formula applied to current CPU, with the same scaling limits. |

Both used a 60% CPU target, 1 to 8 pods with 1 CPU each and a 30-second sync period. They shared the same scaling
limits: when scaling up, at most double or 4 more pods per 60 seconds; when scaling down, any amount per 30 seconds,
with no stabilization window.

These runs also included an earlier HPA+ mode that forecast from measured CPU alone. That mode is being removed from
HPA+, so its results are not shown.

## Results by traffic

Share of requests answered within 500 ms. HPA+ used XGBoost in every row.

| Traffic | HPA+ | HPA formula |
| --- | ---: | ---: |
| [Repeated peaks](synthetic.md#repeated-peaks) (4-hour run) | 99.9% | 28.5% |
| [Cycles](synthetic.md#cycles) (4-hour run) | 99.5% | 83.2% |
| [Sudden jumps](synthetic.md#sudden-jump) (4-hour run) | 92.2% | 81.5% |
| [1998 World Cup](world-cup.md), one day | 99.89% | 99.70% |

On the World Cup day almost every request was answered in time by both, so the misses tell them apart: 204 requests
for HPA+ and 570 for the HPA formula, out of 187,008 each.

## Limits

- **One application.** The test service is CPU-bound. Workloads limited by memory, I/O or downstream services were not tested.
- **The HPA formula setup is not a default Kubernetes HPA.** It reacted every 30 seconds and removed pods without the default 5-minute scale-down stabilization window. A default HPA keeps more pods after each peak; that configuration was not tested.
- **HPA+ needs request metrics.** It reads per-pod request counters from Prometheus and CPU from the HPA+ cgroup collector; see the [request-demand guide](../user-guide/request-demand.md).
- **Scale-up period.** Both setups could add pods once per 60 seconds. The HPA+ default is 15 seconds, as in Kubernetes, which lets both setups grow faster; these runs did not measure it.
- **Fast pod start.** Pods in this test were ready 1 to 3 seconds after they were created. Applications that start slowly leave less time to react.

## How the tests were run

- **Cluster.** The copies of the application ran on three worker nodes of an on-premises Kubernetes cluster. The HPA+ operator, the traffic generator and Prometheus ran on a separate node.
- **Application.** A Python HTTP service with two request types: a checkout does three times the CPU work of a browse. Each pod requests and is limited to 1 CPU and works on at most 4 requests at once. It rejects a request that cannot start within 1 second or that arrives when 64 are already in flight.
- **Traffic.** One open-loop generator sent the same request schedule to each copy at fixed times, whatever the responses. The schedule is described on each traffic page.
- **Scoring.** A request counts as on time when it returns a successful response within 500 ms. Requests time out after 3 seconds. The first 2 minutes of each scored part are not counted.
