# Metrics

`spec.metrics` takes the same metric specs as the Kubernetes HPA, so an existing HPA's metrics can be copied over
as they are. HPA+ uses them for the reactive recommendation, and the models use the CPU target to turn forecasts into
replicas:

```yaml
metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 50
```

At least one metric is required. Resource, Pods, Object and External metrics work for the reactive recommendation;
ContainerResource metrics are not supported. With several metrics, the reactive recommendation takes the highest
replica count, as in the
[Kubernetes HPA walkthrough](https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale-walkthrough/#autoscaling-on-multiple-metrics-and-custom-metrics).

Every model needs a CPU Resource metric with a `Utilization` target and a positive `averageUtilization`. The models
forecast CPU only; the other metrics feed the reactive recommendation. See
[typed forecasting](../reference/typed-forecasting.md).

CPU request accounting includes native sidecars and explicit Pod-level requests. During rollouts with mixed request
sizes, or while VPA changes requests, forecasts are converted with the smallest observed request, capped by a smaller
workload-template request; this can add replicas for a while. A known pending resize makes utilization unavailable.
Incomplete pod or container CPU samples are left out of training and block scale-down. See
[CPU capacity](../reference/cpu-capacity.md).

[`metricMaxAge`](../reference/configuration.md#metricmaxage) limits how old metric timestamps may be, and
[partial metric availability](../reference/configuration.md#partial-metric-availability) explains what happens when
some metrics are missing. [Observability](observability.md) covers the Prometheus metrics about HPA+ itself.
