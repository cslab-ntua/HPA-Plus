# Models

This page documents the model families that are implemented in the current repository.

It is intentionally short. For exact field names and validation details, check [`api/v1alpha1/predictivehorizontalpodautoscaler_types.go`](../../api/v1alpha1/predictivehorizontalpodautoscaler_types.go) and the sample manifests under [`testing/manifests/hpa-plus/`](../../testing/manifests/hpa-plus/).

## How Models Fit Into HPA+

Each entry under `spec.models` contributes a predicted replica count.

Outside the model block:

- `includeHPA` decides whether the plain HPA baseline is also included in the final decision.
- `decisionType` decides how multiple inputs are combined (`maximum`, `minimum`, `mean`, `median`).
- `syncPeriod` controls how often the controller evaluates metrics and models.

In practice this means a model is only one part of the final scaling behavior. Stabilization windows, min/max replicas, and the underlying HPA metric target still matter.

## Shared Model Fields

All model entries support these common fields:

- `type`: model family. Supported values in this repo are `Linear`, `HoltWinters`, `ARIMA`, `XGBoost`, and `LightGBM`.
- `name`: unique model name within the PHPA object.
- `perSyncPeriod`: run cadence in units of `syncPeriod`. `1` means every sync, `2` means every other sync.
- `calculationTimeout`: max time, in milliseconds, allowed for one prediction run.
- `startInterval`: optional delayed start boundary for time-aligned models.
- `resetDuration`: optional idle timeout after which model state/history is reset.

## Training Input
All models in the current repository learn from aggregate CPU usage history stored in the PHPA ConfigMap and convert predicted CPU demand back into replicas using the current CPU request per pod and target utilization.

If a model does not yet have enough usable CPU samples, the controller falls back to the HPA path until the history is warm.

## Linear

Use this when you want the lightest-weight predictive layer and only need short-horizon trend following over CPU demand.

Example:

```yaml
models:
  - type: Linear
    name: simple-linear
    perSyncPeriod: 1
    linear:
      lookAhead: 10000
      historySize: 6
```

Key fields:

- `linear.lookAhead`: forecast horizon in milliseconds.
- `linear.historySize`: number of retained CPU-history samples.

See [`examples/simple-linear/hpa-plus.yaml`](../../examples/simple-linear/hpa-plus.yaml).

## Holt-Winters

Use this when the workload has a strong repeating seasonal CPU pattern and you want explicit trend/seasonality controls.

Example:

```yaml
models:
  - type: HoltWinters
    name: seasonal-predictor
    perSyncPeriod: 1
    startInterval: 60s
    holtWinters:
      alpha: 0.9
      beta: 0.9
      gamma: 0.9
      seasonalPeriods: 6
      storedSeasons: 4
      trend: additive
      seasonal: additive
```

Key fields:

- `holtWinters.alpha`, `beta`, `gamma`: smoothing factors.
- `holtWinters.seasonalPeriods`: season length in sync periods.
- `holtWinters.storedSeasons`: how many seasons to retain.
- `holtWinters.trend`, `seasonal`: additive or multiplicative behavior.
- `holtWinters.runtimeTuningFetchHook`: optional hook-based runtime tuning.

See:

- [`examples/simple-holt-winters/hpa-plus.yaml`](../../examples/simple-holt-winters/hpa-plus.yaml)
- [`examples/dynamic-holt-winters/hpa-plus.yaml`](../../examples/dynamic-holt-winters/hpa-plus.yaml)
- [hooks.md](./hooks.md)

## ARIMA

Use this when you want a classical time-series model with explicit order control, optional SARIMA seasonality, or incremental updates between syncs.

Example:

```yaml
models:
  - type: ARIMA
    name: traffic-predictor
    perSyncPeriod: 1
    calculationTimeout: 180000
    arima:
      order: [2, 0, 3]
      lookAhead: 60000
      historySize: 400
      autoArima: false
      trend: "c"
      useSarima: false
      incrementalUpdates: true
      refitEvery: 20
```

Key fields:

- `arima.order`: `[p, d, q]`.
- `arima.lookAhead`: forecast horizon in milliseconds.
- `arima.historySize`: retained CPU-history samples.
- `arima.autoArima`: let the model choose order automatically.
- `arima.useSarima`, `seasonalOrder`, `seasonalPeriods`: seasonal ARIMA path.
- `arima.incrementalUpdates`, `refitEvery`: stateful incremental runtime behavior.

See [`arima.yaml`](../../testing/manifests/hpa-plus/arima.yaml).

## XGBoost

Use this when you want a tree-based model over CPU history with more nonlinear fitting capacity than the simpler statistical models.

Example:

```yaml
models:
  - type: XGBoost
    name: cpu-predictor
    perSyncPeriod: 1
    calculationTimeout: 60000
    xgboost:
      historySize: 400
      lookAhead: 60000
      lags: 128
      windowSize: 128
      nEstimators: 200
      maxDepth: 5
      learningRate: 0.05
      subsample: 1.0
      colsampleBytree: 1.0
      minChildWeight: 5
      regLambda: 1
```

Key fields:

- `xgboost.historySize`: retained CPU-history samples.
- `xgboost.lags`, `windowSize`: feature-history depth.
- `xgboost.nEstimators`, `maxDepth`, `learningRate`: core boosting controls.
- `xgboost.subsample`, `colsampleBytree`: row/feature sampling.
- `xgboost.minChildWeight`, `gamma`, `regLambda`, `regAlpha`: regularization controls.

See [`xgboost.yaml`](../../testing/manifests/hpa-plus/xgboost.yaml).

## LightGBM

Use this when you want the same CPU-history approach as XGBoost but with the LightGBM tree implementation and its own tuning knobs.

Example:

```yaml
models:
  - type: LightGBM
    name: cpu-predictor
    perSyncPeriod: 1
    calculationTimeout: 60000
    lightgbm:
      historySize: 400
      lookAhead: 60000
      lags: 128
      windowSize: 128
      nEstimators: 200
      maxDepth: -1
      learningRate: 0.1
      subsample: 1.0
      colsampleBytree: 1.0
      numLeaves: 31
      minChildSamples: 20
      regLambda: 0
      regAlpha: 0
```

Key fields:

- `lightgbm.historySize`: retained CPU-history samples.
- `lightgbm.lags`, `windowSize`: feature-history depth.
- `lightgbm.nEstimators`, `maxDepth`, `learningRate`: core boosting controls.
- `lightgbm.subsample`, `colsampleBytree`: row/feature sampling.
- `lightgbm.numLeaves`, `minChildSamples`: LightGBM tree-shape controls.
- `lightgbm.regLambda`, `regAlpha`: regularization controls.

See [`lightgbm.yaml`](../../testing/manifests/hpa-plus/lightgbm.yaml).

## Picking A Starting Point

Use this rule of thumb:

- Start with `Linear` if you want the simplest CPU-history predictive behavior.
- Use `HoltWinters` if the workload is strongly seasonal and interpretable season controls matter.
- Use `ARIMA` if you want a classical time-series model with explicit order control or incremental updates.
- Use `XGBoost` or `LightGBM` if you are tuning against recorded CPU history and want higher-capacity nonlinear models.

For the tree-based models in this repo, the practical workflow is:

1. Capture realistic runtime history.
2. Benchmark candidate parameter grids with the scripts under [`scripts/`](../../scripts/).
3. Apply the chosen settings in the corresponding manifest under [`testing/manifests/hpa-plus/`](../../testing/manifests/hpa-plus/).
