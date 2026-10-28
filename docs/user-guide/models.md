# Models

Each entry in `spec.models` forecasts the workload's CPU demand. HPA+ turns every forecast into a replica count with
the pods' CPU request and the utilization target, then combines the counts into one decision. This page shows how to
configure each model. The exact field names and bounds are in
[`api/v1alpha1/hpaplus_types.go`](https://github.com/cslab-ntua/HPA-Plus/blob/master/api/v1alpha1/hpaplus_types.go),
and [forecast policy](../reference/forecast-policy.md) explains how forecasts are timed, reused and checked.

## How models fit into HPA+

Each model contributes a predicted replica count. Outside the model block:

- `decisionType` decides how several counts are combined (`maximum`, `minimum`, `mean`, `median`).
- `includeHPA` decides whether the reactive HPA recommendation is combined with them.
- `predictionFailurePolicy` decides what happens when no model has a usable forecast: the reactive recommendation
  (`Reactive`, the default) or the current replica count (`HoldCurrent`).
- `syncPeriod` sets how often HPA+ samples CPU and decides.

A model is only one part of the final decision: stabilization windows, rate limits, `minReplicas`, `maxReplicas` and
the CPU target still apply. Start a new model in [observe mode](rollout.md) and compare its recommendations with real
traffic before giving it control.

## Shared fields

Every model entry has these fields:

- `type`: `Linear`, `HoltWinters`, `ARIMA`, `XGBoost` or `LightGBM`.
- `name`: unique within the HPAPlus.
- `perSyncPeriod`: new samples needed after the last successful run before the model runs again. Default `1`; with
  regular sampling, `2` runs it about every other sync. It cannot exceed the retained history.
- `calculationTimeout`: the longest one run may take, in milliseconds. Default `30000`.
- `predictionMaxAge`: optional. Lets a successful forecast be reused for this many milliseconds after its newest
  training sample, but never past the forecast's own expiry. Without it, only fresh forecasts are used.
- `historyMaxAge`: optional. Drops training samples older than this many milliseconds; without it, only the sample
  count limits the window. See [training data age](../reference/forecast-policy.md#training-data-age).
- `startInterval`: optional delayed start boundary for time-aligned models.
- `resetDuration`: optional. Resets the model's history after it has been idle this long.
- `predictivePolicy`: optional [timed decisions](../reference/forecast-policy.md#timed-decisions).
- `forecastReliability`: optional [reliability checks](../reference/forecast-policy.md#reliability-checks).
- `requestDemand`: XGBoost only, see [request demand](request-demand.md).

Choose a `lookAhead` longer than the age of the newest training sample plus the fitting time, or the forecast can
expire before it is used. [Expiry and reuse](../reference/forecast-policy.md#expiry-and-reuse) has the details.

## Training data

Models learn from the workload's aggregate CPU usage, which the history sampler records in the HPA+ ConfigMaps. Each
row keeps the time of the CPU measurement. A missing pod or container leaves a gap instead of a lower number.
Complete CPU from starting pods is kept for training, even when the reactive HPA calculation skips it.

CPU comes from the Metrics Server by default. The operator setting `cpuHistorySource: kubelet` (flag
`--cpu-history-source=kubelet`) reads container CPU rates from kubelet summaries instead. It needs `get` on
`nodes/proxy`, which the chart grants only when this source is selected, and every serving container must report.
It changes only the training data; the reactive recommendation still uses the Metrics API. `cpuHistorySource: cgroup`
reads the [cgroup collector](request-demand.md#installation-and-measurements) for both. A measured zero is valid; a
missing or stale rate is not. Whatever the source, CPU near an enforced limit can hide demand that the application
turned away.

Until a model has enough usable samples it has no forecast, and HPA+ follows `predictionFailurePolicy`. Warm-up never
produces a zero forecast. A real zero-demand forecast is valid, subject to `minReplicas` and the scaling behavior.

A malformed training window, for example with missing or duplicate timestamps or negative CPU, is dropped and then
rebuilt by sampling. Timestamps in the future pause sampling and forecasts with `HistoryClockSkew` until the clock
catches up, and the history is kept. Other models' histories and pending scale accounting are kept too. See
[history corruption and recovery](../reference/history-recovery.md).

Failed fits are retried with growing delays, from `syncPeriod` up to five minutes by default. See
[retry behavior](../reference/model-runtime.md#retry-circuit-behavior).

## Linear

Use this when you want the lightest predictive layer and only need short-horizon trend following over CPU demand.

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

- `linear.lookAhead`: forecast horizon in milliseconds after the newest CPU sample. The Go adapter sends this anchor
  explicitly, so queue delays and Python startup don't extend the horizon. Timestamps are UTC.
- `linear.historySize`: number of retained CPU samples.

## Holt-Winters

Use this when the workload has a strong repeating CPU pattern and you want explicit trend and seasonality controls.

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
- `holtWinters.seasonalPeriods`: season length in samples.
- `holtWinters.storedSeasons`: how many seasons to retain.
- `holtWinters.trend`, `seasonal`: additive or multiplicative behavior.
- `holtWinters.lookAhead`: optional horizon in milliseconds. Without it, Holt-Winters forecasts one step ahead.
- `holtWinters.dampedTrend`: `true` estimates a damping factor during fitting, keeping the supplied `alpha`, `beta`
  and `gamma` fixed. Estimated initialization can fit the initial states too; `initializationMethod: known` keeps the
  supplied ones. Without damping, the supplied factors are used without optimization. See the
  [statsmodels fit documentation](https://www.statsmodels.org/stable/generated/statsmodels.tsa.holtwinters.ExponentialSmoothing.fit.html).
- `holtWinters.runtimeTuningFetchHook`: optional HTTP hook that supplies the smoothing factors at runtime. The
  operator must allow the hook's origin in `hooks.allowedOrigins`. See [hooks](hooks.md).

## ARIMA

Use this when you want a classical time-series model with explicit order control, optional SARIMA seasonality, or
incremental updates between syncs.

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
- `arima.historySize`: retained CPU samples.
- `arima.autoArima`: let the model choose the order.
- `arima.useSarima`, `seasonalOrder`, `seasonalPeriods`: seasonal ARIMA.
- `arima.incrementalUpdates`, `refitEvery`: keep the fitted model between runs, extend it with every new sample and
  refit every `refitEvery` samples. The worker keeps a recent timing window the size of its last full fit and uses
  its median interval to work out how many steps reach the horizon. A failed forecast commits no partial fit or
  append.

Fit failures and empty or non-finite forecasts count as failures; the last CPU observation is never used as a
stand-in forecast. Automatic order selection ignores non-finite information criteria and fails if no configuration
works.

## XGBoost

Use this when you want a tree model over CPU history with more nonlinear capacity than the statistical models.

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

- `xgboost.historySize`: retained CPU samples.
- `xgboost.lags`, `windowSize`: feature history depth.
- `xgboost.nEstimators`, `maxDepth`, `learningRate`: core boosting controls.
- `xgboost.subsample`, `colsampleBytree`: row and feature sampling.
- `xgboost.minChildWeight`, `gamma`, `regLambda`, `regAlpha`: regularization.
- `xgboost.objective: reg:quantileerror` with `quantileAlpha` strictly between zero and one fits one conditional
  CPU-usage quantile. For example, `0.9` penalizes underprediction more than overprediction. It is one XGBoost model,
  not an ensemble or a calibrated confidence guarantee. Other objectives must omit `quantileAlpha`. Higher quantiles
  can reserve more CPU; check utilization and application outcomes together.
- `xgboost.forecastAggregation`: `last` (the default) uses the final forecast step. `maximum` uses the highest
  predicted CPU among the steps up to the horizon. Neither adds the reactive recommendation or the observed CPU as a
  floor; cache expiry, replica bounds and scaling behavior still apply. Experimental: check it against the workload's
  scale-down policy before using it.
- `xgboost.seasonalFeatures`: optional, default `false`. Adds target-time phase, sine and cosine features. The cycle
  is estimated only from past CPU samples; no traffic schedule is supplied. If no reliable cycle is found, the three
  features are missing. Experimental: irregular history, saturation and changing demand can make a past cycle
  misleading.
- `xgboost.forecastStrategy`: `recursive` (the default) or the experimental `direct`. Direct fits one XGBoost model
  with a lead-time feature, trained on several observed targets ahead of each historical sample, and predicts every
  step from the same observed lag window instead of feeding predictions back in. Missing targets are left out, never interpolated, and the larger
  matrices count against the [work budgets](../reference/model-runtime.md#training-and-search-budgets). Direct
  forecasts also get CPU trends per second and target-time projections from the last three, six and `windowSize`
  samples (distinct lookbacks only; windows under three samples keep the basic features). These are causal inputs:
  measured targets are unchanged and the regressor still makes the forecast. They can reduce errors when CPU changes,
  but don't guarantee accurate ramps or unforeseen bursts. `forecastAggregation` works with both strategies.

Experimental options:

- `cpuDemandWeightExponent`: gives higher-CPU training targets more weight in the loss. A finite value from `0` to
  `2`; omitted or `0` trains unweighted. For an exponent `p`, each target `y` gets the weight
  `clip((max(y, 1) / max(median(targets), 1))^p, 0.5, 4)`, with CPU in millicores. Targets, timestamps and missing
  data are unchanged. Emphasizing rare peaks can also increase overprediction, so tune it against application
  outcomes and the capacity it adds.
- `objective: survival:aft` with `forecastStrategy: direct` treats CPU near saturation as a lower bound on demand. It
  needs `aftLossDistributionScale` (0.05–2) and `cpuCensoringUtilizationThreshold` (90–10000, percent of the CPU
  request). Use it only when the threshold matches capacity that is really enforced: 90% can mark near-saturation when
  CPU limits equal requests, but 90% of the request alone doesn't prove saturation for a workload without limits.
  Targets at or above the threshold become lower bounds in XGBoost's AFT objective; other targets stay exact. Training
  adds a one-millicore offset because AFT needs positive labels, and predictions remove it. Measurements are never
  overwritten. It needs utilization history and cannot be combined with `quantileAlpha`. The uncertainty scale affects
  extrapolation and can cause large overprovisioning.

## LightGBM

Use this when you want the same CPU-history approach as XGBoost with LightGBM's tree implementation and tuning knobs.

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

- `lightgbm.historySize`: retained CPU samples.
- `lightgbm.lags`, `windowSize`: feature history depth.
- `lightgbm.nEstimators`, `maxDepth`, `learningRate`: core boosting controls. `maxDepth: -1` means no depth limit.
- `lightgbm.subsample`, `colsampleBytree`: row and feature sampling.
- `lightgbm.numLeaves`, `minChildSamples`: tree shape.
- `lightgbm.regLambda`, `regAlpha`: regularization.

## Options for both tree models

`useUtilizationFeatures: false` inside the `xgboost` or `lightgbm` block leaves out the optional CPU usage-to-request
lag features and keeps the aggregate CPU history. Omitted or `true` keeps them. Utilization changes whenever capacity
changes, so CPU-only features can reduce feedback from the scaler's own actions; check accuracy and capacity on your
workload. The CPU request and target still convert forecasts into replicas. XGBoost's `survival:aft` objective needs
utilization history and rejects `false`.

Tree feature snapshots for debugging are described in [feature diagnostics](../reference/feature-diagnostics.md).

## Choosing a model

- Start with `Linear` for the simplest CPU-history forecasts.
- Use `HoltWinters` when the workload is strongly seasonal and readable season controls matter.
- Use `ARIMA` for a classical time-series model with explicit order control or incremental updates.
- Use `XGBoost` or `LightGBM` when you tune against recorded CPU history and want higher-capacity nonlinear models.

For the tree models, a practical workflow is:

1. Capture realistic runtime history.
2. Compare parameter grids against that history outside the cluster.
3. Apply the chosen settings in the `HPAPlus` model block.

## Validation

The controller checks these rules on top of the bounds in the CRD schema:

| Model | Required bounds and relationships |
| --- | --- |
| All | A CPU `Utilization` metric with positive `averageUtilization`; unique nonblank name; exactly one matching configuration block; positive timeouts, intervals, and `perSyncPeriod`. |
| Linear | `historySize >= 2`, `lookAhead >= 1` millisecond. |
| Holt-Winters | `seasonalPeriods >= 2`; positive `storedSeasons`; their product must be at least `max(2 * seasonalPeriods, 10 + 2 * floor(seasonalPeriods / 2))`. Supply `alpha`, `beta`, and `gamma` in `[0, 1]` or a runtime tuning hook. |
| ARIMA | Three non-negative integers in each supplied order; `historySize >= 3` (default `50`); positive `lookAhead` and `refitEvery`. Seasonal mode requires both a three-element `seasonalOrder` and `seasonalPeriods >= 2`, including when either seasonal field enables the mode implicitly. `autoArima` and `incrementalUpdates` cannot both be true. |
| Both trees | `1 <= lags < historySize`; `1 <= windowSize <= lags` when supplied; positive `lookAhead`, `nEstimators`, and `learningRate`; `subsample` and `colsampleBytree` in `(0, 1]`; non-negative regularization. |
| XGBoost | Positive `maxDepth`; `minChildWeight` and `gamma` may be zero. Explicit zero regularization is preserved. |
| LightGBM | `maxDepth` is `-1` or positive; `numLeaves >= 2`; `minChildSamples >= 1`. Zero depth is rejected by the controller. |

`perSyncPeriod` cannot exceed the retained history: `historySize` for Linear, ARIMA and the trees, or
`seasonalPeriods * storedSeasons` for Holt-Winters. With six samples per season, Holt-Winters needs at least three
stored seasons to pass its initialization minimum. These checks catch configurations that could never train; they
don't guarantee that a fit converges or forecasts well.

A resource that fails these checks is stored by the API server, but HPA+ doesn't scale it until it is fixed. The
`ConfigurationValid` condition and the operator logs say why.
