# HPA+

HPA+ adds CPU-demand forecasting to Kubernetes horizontal autoscaling. Its model suite includes Linear, Holt-Winters, ARIMA/SARIMA, XGBoost and LightGBM. Each model contributes a replica recommendation within shared workload identity, freshness, resource and scaling controls.

- [Install the operator](user-guide/installation.md).
- [Configure an existing workload](user-guide/getting-started.md).
- Read the [configuration reference](reference/configuration.md) and [model guide](user-guide/models.md).
- Understand [rollout modes](user-guide/rollout.md), [CPU capacity](reference/cpu-capacity.md) and [forecast validity](reference/forecast-policy.md).
- Inspect [status and metrics](user-guide/observability.md), [operations](user-guide/operations.md) and [architecture](architecture.md).
- Watch decisions, CPU and forecasts in the [local UI](user-guide/ui.md) with `make ui`.
- See how request-demand forecasting performed in the [benchmark results](results/README.md).

HPA+ forecasts measured aggregate CPU usage. CPU forecasts alone do not see demand that saturated pods could not serve; the opt-in [request-demand mode](user-guide/request-demand.md) rebuilds that demand from request counters. Fitting a model successfully does not establish its accuracy or application performance.
