# Getting started

Install the operator using the [installation guide](installation.md). Select an existing Deployment whose serving containers have positive CPU requests and whose resource metrics are available. The HPAPlus and target must be in the same namespace.

Create an HPAPlus for that workload, replacing the namespace and deployment name:

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
  decisionType: maximum
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: 60
  models:
    - name: linear
      type: Linear
      linear:
        historySize: 12
        lookAhead: 60000
```

Save this as `hpaplus.yaml` and apply it:

```bash
kubectl apply -f hpaplus.yaml
kubectl -n application-namespace get hpaplus application -o yaml
```

This configuration collects CPU history and publishes recommendations in observe mode. Its values illustrate the resource shape; choose a model, history and horizon appropriate to your application. Twelve complete observations at a 30-second cadence require approximately six minutes to accumulate, with longer warm-up when measurements are missing.

Inspect conditions and model status for configuration errors, missing CPU, warm-up and expired forecasts. Compare recommendations with actual workload behavior before setting `spec.mode: control`. Ensure only one autoscaler writes the Deployment's replicas when control is enabled.

See [model configuration](models.md), [rollout modes](rollout.md), [configuration reference](../reference/configuration.md) and [operating HPA+](operations.md).
