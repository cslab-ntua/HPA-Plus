# Metrics

Deciding which metrics to use is done by using `MetricSpecs`, which are a key part of HPAs, and look like this:

```yaml
- type: Resource
  resource:
    name: cpu
    target:
      type: Utilization
      averageUtilization: 50
```

To send these specs to HPA+, add a `metrics` list to the `HPAPlus` spec. For example:

```yaml
metrics:
- type: Resource
  resource:
    name: cpu
    target:
      type: Utilization
      averageUtilization: 50
```

This allows porting over existing Kubernetes HPA metric configurations to the HPA+.
Equivalent to K8s HPA metric specs; which are [demonstrated in this HPA
walkthrough](https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale-walkthrough/#autoscaling-on-multiple-metrics-and-custom-metrics).
Can hold multiple values as it is an array.

HPA+ currently supports reactive Resource, Pods, Object and External metrics. ContainerResource metrics are not
supported. Predictive models require a CPU Resource metric with an AverageUtilization target.

See [metric freshness and status](../reference/configuration.md#metric-source-freshness) for age limits and partial
availability behavior, and [operator observability](observability.md) for Prometheus metrics about HPA+ itself.

Predictive model support is explicitly CPU-only; see the [typed demand/capacity contract](../reference/typed-forecasting.md). Other supported metric sources feed the baseline.

CPU request accounting includes native sidecars and explicit Pod-level requests. During mixed-size rollouts or VPA
changes, predictive conversion uses the smallest observed request, capped by a smaller workload-template request;
this can temporarily favor extra replicas. Known pending resizes make utilization unavailable. Incomplete pod or
container CPU samples are excluded from training and block downscaling. See [CPU capacity](../reference/cpu-capacity.md)
for the policy, compatibility and test scope.
