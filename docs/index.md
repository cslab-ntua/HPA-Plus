# HPA+

HPA+ adds CPU-demand forecasting to Kubernetes horizontal autoscaling. By default it rebuilds the demand that full pods hide from request counters and forecasts that. Its model suite includes Linear, Holt-Winters, ARIMA/SARIMA, XGBoost, LightGBM, Toto, TiRex and a seasonal naive baseline. Each model contributes a replica recommendation within shared workload identity, freshness, resource and scaling controls.

- [Install the operator](user-guide/installation.md).
- [Configure an existing workload](user-guide/getting-started.md).
- Read the [configuration reference](reference/configuration.md) and [model guide](user-guide/models.md).
- Understand [rollout modes](user-guide/rollout.md), [CPU capacity](reference/cpu-capacity.md) and [forecast validity](reference/forecast-policy.md).
- Inspect [status and metrics](user-guide/observability.md), [operations](user-guide/operations.md) and [architecture](architecture.md).
- Watch decisions, CPU and forecasts in the [local UI](user-guide/ui.md) with `make ui`.
- See how request-demand forecasting performed in the [benchmark results](results/README.md).

Measured CPU stops at what full pods can do, so it does not show demand they could not serve. [Request demand](user-guide/request-demand.md), the default, rebuilds that demand from request counters. Workloads without request counters can forecast measured CPU only with `cpuOnly: true`, which does much worse. Fitting a model successfully does not establish its accuracy or application performance.
