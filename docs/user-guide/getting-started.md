# Getting started

Install the operator using the [installation guide](installation.md), with `trafficPrometheusURL` set to the
Prometheus that scrapes your application. Select an existing Deployment whose serving containers have positive CPU
requests. The HPAPlus and target must be in the same namespace.

HPA+ forecasts the demand rebuilt from two per-pod request counters, requests started and requests
finished. [Request demand](request-demand.md) lists what it needs. Create an HPAPlus for that workload, replacing the
namespace, deployment name and metric names:

```yaml
apiVersion: hpa.plus/v1alpha1
kind: HPAPlus
metadata:
  name: application
  namespace: application-namespace
spec:
  mode: observe
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: application
  minReplicas: 1
  maxReplicas: 8
  syncPeriod: 30000
  includeHPA: false
  predictionFailurePolicy: HoldCurrent
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: 60
  models:
    - name: demand
      type: Linear
      predictionMaxAge: 120000
      predictivePolicy:
        scaleUpLeadSeconds: 15
      linear:
        historySize: 20
        lookAhead: 90000
      requestDemand:
        traffic:
          provider: prometheus
          arrivalsMetric: app_requests_started_total
          completionsMetric: app_requests_finished_total
          arrivalSemantics: started
          completionSemantics: finished
          retrySemantics: attempts
```

Save this as `hpaplus.yaml` and apply it:

```bash
kubectl apply -f hpaplus.yaml
kubectl -n application-namespace get hpaplus application -o yaml
```

This configuration collects CPU history and request counts and publishes recommendations in observe mode. Its values
illustrate the resource shape; choose a model, history and horizon appropriate to your application. Twenty complete
windows at a 30-second cadence take about ten minutes to accumulate, with longer warm-up when measurements are missing.

Inspect conditions and model status for configuration errors, missing CPU or request counts, warm-up and expired
forecasts. Compare recommendations with actual workload behavior before setting `spec.mode: control`. Ensure only one
autoscaler writes the Deployment's replicas when control is enabled.

See [model configuration](models.md), [rollout modes](rollout.md), [configuration reference](../reference/configuration.md) and [operating HPA+](operations.md).
