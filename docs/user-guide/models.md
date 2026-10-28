# Models

This page documents the model families that are implemented in the current repository.

It is intentionally short. For exact field names and validation details, check [`api/v1alpha1/hpaplus_types.go`](https://github.com/cslab-ntua/HPA-Plus/blob/master/api/v1alpha1/hpaplus_types.go).

## How Models Fit Into HPA+

Each entry under `spec.models` contributes a predicted replica count.

Outside the model block:

- `includeHPA` decides whether the plain HPA baseline is also included in the final decision.
- `decisionType` decides how multiple inputs are combined (`maximum`, `minimum`, `mean`, `median`).
- `syncPeriod` controls how often the controller evaluates metrics and models.

In practice this means a model is only one part of the final scaling behavior. Stabilization windows, min/max replicas, and the underlying HPA metric target still matter.

Start new model configurations in [observe mode](rollout.md) and evaluate their recommendations with representative application traffic before enabling control.

Holt-Winters `dampedTrend: true` estimates the damping coefficient during fitting while keeping supplied `alpha`,
`beta` and `gamma` fixed. Estimated initialization may also fit initial states; `initializationMethod: known` keeps
supplied initial states fixed. Undamped fitting retains its previous non-optimized behavior. See the
[statsmodels fit contract](https://www.statsmodels.org/stable/generated/statsmodels.tsa.holtwinters.ExponentialSmoothing.fit.html).

## Shared Model Fields

All model entries support these common fields:

- `type`: model family. Supported values in this repo are `Linear`, `HoltWinters`, `ARIMA`, `XGBoost`, and `LightGBM`.
- `name`: unique model name within the `HPAPlus` object.
- `perSyncPeriod`: number of new retained samples required after the last successful model run; defaults to `1`.
  With regular sampling, `2` approximates every other sync. It must not exceed retained history capacity.
- `calculationTimeout`: max time, in milliseconds, allowed for one prediction run.
- `predictionMaxAge`: optional positive milliseconds from the latest consumed sample during which a successful
  prediction may be reused. Omit it for fresh-only behavior. Fresh and cached production forecasts expire at their
  data-relative target; explicit `lookAhead` remains a conservative ceiling. Implicit Holt also retains the
  `syncPeriod` reuse cap; its actual one-step target may shorten validity further. Model/specification changes, CPU request/target changes, and history resets invalidate reuse.
- `historyMaxAge`: optional positive milliseconds limiting training sample age; unset keeps count-only retention.
- `startInterval`: optional delayed start boundary for time-aligned models.
- `resetDuration`: optional idle timeout after which model state/history is reset.

Choose `lookAhead` longer than the age of the latest training sample plus fitting time. The independent sampler may
not have completed its newest slot when reconciliation starts. Expired fits do not consume history or enter backoff.
[Forecast policy](../reference/forecast-policy.md) describes exact deadlines and point-only uncertainty semantics.

For example, with `perSyncPeriod: 2`, `predictionMaxAge: 45000`, and `linear.lookAhead: 60000`, a successful
prediction can contribute between fits for up to 45 seconds from its latest training sample. The same limit applies
after a fit failure. A slow fit does not receive a new freshness window when it completes. Sampling continues
independently, and cached results do not advance the consumed-sample marker. Missing, expired, or invalidated results
leave the HPA baseline available as fallback. Cache writes must succeed before a newly computed reusable result is used. If any reusable candidate expires during
decision persistence, the controller reevaluates before sending a scale change. This also guards held-decision status
retries so expired recommendations cannot enter stabilization history. `Expired` model status retains consumed/expiry
timestamps but has no usable replica count; fit failures and backoff remain the primary reason when present.

## Training Input
All models in the current repository learn from aggregate CPU usage history stored in the HPA+ ConfigMap and convert predicted CPU demand back into replicas using the current CPU request per pod and target utilization.

Training records retain the CPU observation timestamp, separately from the sampling slot. Missing pods or containers
leave gaps. Complete measured startup CPU can enter training even when native HPA initialization filters exclude it
from reactive evaluation. An optional operator `--cpu-history-source=kubelet` (Helm `cpuHistorySource: kubelet`) reads
genuine container CPU rates from kubelet summaries and collects training independently of reactive evaluation.
It requires `get` on `nodes/proxy`; Helm grants that permission only when enabled. The default is `metrics-server`.
Kubelet training joins the selected namespace/pod UID and requires every serving container. Missing or stale rates
are unavailable, whereas a measured zero is valid. Native reactive metrics and recommendations stay unchanged.
CPU consumption near an enforced limit can still understate demand rejected by the application; a fresher source
does not recover that unobserved demand.

If a model does not yet have enough usable CPU samples, it reports an unavailable forecast and the controller uses the
normal cache/HPA fallback policy. Warmup never contributes a zero or historical replica count as a forecast. A genuine
zero-demand forecast remains valid, subject to `minReplicas` and scaling behavior.

Structurally malformed training windows (for example missing/duplicate timestamps or negative CPU usage) are excluded
and reset for that model. Sampling then rebuilds the window. Otherwise valid future timestamps report
`HistoryClockSkew` and preserve the window while sampling/forecasts pause until clock recovery. Other models and
pending scale accounting are retained. See [history corruption and recovery](../reference/history-recovery.md).

Repeated failed fits use exponential retry delays: start at `syncPeriod`, with a minimum of one second and a maximum
of five minutes, then double up to that maximum. Operator `modelRetry.initial` and `modelRetry.maximum` Helm values override these defaults (see [runtime controls](../reference/model-runtime.md)). Retry timestamps round up to whole seconds. Sampling continues, and
normal cache/HPA fallback applies during the wait. A successful fit clears the failure count. Changing model
configuration or CPU capacity permits an immediate eligible attempt; warmup and operator shutdown do not increase the
failure count. Per-model health and retry timing are exposed in status.

## Configuration Constraints

The controller validates these relationships in addition to the scalar bounds enforced by the CRD:

| Model | Required bounds and relationships |
| --- | --- |
| All | A CPU `Utilization` metric with positive `averageUtilization`; unique nonblank name; exactly one matching configuration block; positive timeouts, intervals, and `perSyncPeriod`. |
| Linear | `historySize >= 2`, `lookAhead >= 1` millisecond. |
| Holt-Winters | `seasonalPeriods >= 2`; positive `storedSeasons`; their product must be at least `max(2 * seasonalPeriods, 10 + 2 * floor(seasonalPeriods / 2))`. Supply `alpha`, `beta`, and `gamma` in `[0, 1]` or a runtime tuning hook. |
| ARIMA | Three non-negative integers in each supplied order; `historySize >= 3` (default `50`); positive `lookAhead` and `refitEvery`. Seasonal mode requires both a three-element `seasonalOrder` and `seasonalPeriods >= 2`, including when either seasonal field enables the mode implicitly. `autoArima` and `incrementalUpdates` cannot both be true. |
| Both trees | `1 <= lags < historySize`; `1 <= windowSize <= lags` when supplied; positive `lookAhead`, `nEstimators`, and `learningRate`; `subsample` and `colsampleBytree` in `(0, 1]`; non-negative regularization. |
| XGBoost | Positive `maxDepth`; `minChildWeight` and `gamma` may be zero. Explicit zero regularization is preserved. |
| LightGBM | `maxDepth` is `-1` or positive; `numLeaves >= 2`; `minChildSamples >= 1`. Zero depth is rejected by the controller. |

`perSyncPeriod` cannot exceed retained history: `historySize` for Linear, ARIMA, and trees, or
`seasonalPeriods * storedSeasons` for Holt-Winters. With six samples per season, Holt-Winters needs at least three stored
seasons to pass its initialization minimum. These checks prevent permanently untrainable configurations; they do not
guarantee fit convergence or forecast quality for every dataset.

ARIMA/SARIMA fit failures and empty or non-finite forecasts are reported as failures; the last CPU observation is not
substituted as a successful forecast. Automatic order selection ignores non-finite information criteria and fails if
no usable configuration is found. These errors follow the controller retry/fallback policy.

When no model contributes a prediction in a cycle, the HPA baseline is used, including cadence skips after warmup.
See [configuration validation and upgrades](../reference/configuration.md#validation-and-upgrades) before upgrading.

## Forecast timestamps and horizons

Timestamped Python models accept timezone-qualified RFC3339 and RFC3339Nano, retaining nanosecond ordering. Offsets
are normalized to the same instant; process time zones and daylight-saving transitions do not change sample spacing.
The Go controller currently persists and sends training samples at whole-second precision.

Linear forecasts at exactly the latest CPU sample plus lookAhead. ARIMA/SARIMA (both execution modes), XGBoost and
LightGBM estimate the interval from the exact median positive sample spacing, with a 1ms minimum.
They forecast `ceil(lookAhead / interval)` steps. For example, a 15-second horizon with 10-second samples predicts two
steps ahead. This replaces nearest-step rounding, which could stop before the requested horizon. Cache expiry remains
bounded by the configured lookAhead; rounding the model's discrete horizon up does not extend reuse.

Holt-Winters accepts optional `holtWinters.lookAhead` in milliseconds and uses the same discrete horizon rule.
Its Go adapter sends timestamps paired with the sorted CPU series. Omission preserves one forecast step and the
existing syncPeriod cache-expiry cap; the actual one-step forecast target can shorten freshness/reuse further. An explicit
horizon caps freshness at lookAhead and reuse at min(predictionMaxAge, lookAhead).

Irregular observations are accepted. Discrete models treat the retained values as a virtual uniform grid at their
median interval, without interpolating, duplicating or filling missing CPU values. Seasonal periods count observations,
not elapsed wall time. Large gaps can therefore distort seasonal behavior; use historyMaxAge/resetDuration to bound
which observations remain useful. Linear instead fits the actual elapsed timestamps. This policy preserves existing
irregular-history behavior while making the approximation explicit. No point forecast carries a calibrated confidence
interval or confidence score. Missing, failed or expired forecasts are unavailable, never zero-demand evidence;
partial-metric/downscale guards and configured model combination rules remain in force. Confidence-based weighting
requires the separate walk-forward quality work, not an invented score.

Missing-timestamp legacy ARIMA one-shot calls preserve
input order and use a 1-second interval fallback; validated controller training histories always require timestamps.

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

- `linear.lookAhead`: forecast horizon in milliseconds after the latest CPU training sample. The Go adapter passes
  this anchor explicitly, so queue delays and Python startup do not extend the requested horizon. Timestamps are UTC.
- `linear.historySize`: number of retained CPU-history samples.

Use this block inside `spec.models` on an `HPAPlus` resource.

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
- `holtWinters.runtimeTuningFetchHook`: optional hook-based runtime tuning. The operator must allow the hook's exact
  origin through `hooks.allowedOrigins`; the default denies hook requests. Redirects and URL credentials are rejected.
  See [hook security](../reference/hook-security.md) before upgrading a hook-enabled deployment.

For hook-based runtime tuning details, see [hooks.md](./hooks.md).

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
- `arima.incrementalUpdates`, `refitEvery`: stateful incremental runtime behavior. The worker retains a recent
  timing window the size of its last full fit, even when periodic refits are disabled. Its recent median interval
  determines how many steps reach the requested horizon. Every new observation updates
  the fitted state; timing metadata no longer grows for the life of the process. Failed forecasts do not commit
  partial fit/append progress.

Use this block inside `spec.models` on an `HPAPlus` resource.

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
- `xgboost.objective: reg:quantileerror` with `quantileAlpha` strictly between
  zero and one fits one conditional CPU-usage quantile. For example, `0.9`
  penalizes underprediction more heavily than overprediction. It is one
  XGBoost model, not an ensemble or a calibrated confidence guarantee. Other
  objectives must omit `quantileAlpha`. Higher quantiles can reserve more CPU;
  validate utilization and application outcomes together.
- `xgboost.forecastAggregation`: `last` (the default) uses the final forecast
  step. `maximum` uses the highest predicted CPU usage among the steps up
  to the configured horizon. It uses one XGBoost model and does not include a
  reactive recommendation or the observed CPU as a lower bound. Cache freshness,
  replica bounds and scaling behavior still apply. This option is experimental;
  evaluate it with the workload's downscale policy before adopting it.
- `xgboost.seasonalFeatures`: optional, default `false`. Adds target-time phase,
  sine and cosine features to the same XGBoost model. The cycle is estimated
  solely from the supplied past CPU samples; no traffic schedule is supplied.
  If no reliable cycle is found, these three features are missing. This is
  experimental: irregular history, saturation and changing demand can make a
  past cycle misleading. Existing lag features and scaling safeguards remain.
- `xgboost.forecastStrategy`: `recursive` preserves the existing default.
  Experimental `direct` fits one scalar XGBoost with a lead-time feature using
  multiple observed historical targets. It predicts future steps from the same
  observed lag window, without feeding its predictions back into later steps.
  Missing target observations are omitted; no values are interpolated. The
  expanded training and inference matrices remain subject to work budgets.
  Direct forecasts also include CPU trends per elapsed second and target-time
  projections from the last three, six and configured window samples (distinct
  lookbacks only). These are causal model inputs; measured CPU targets stay
  unchanged and the regressor still produces the forecast. Windows smaller than
  three samples retain the original features. These inputs can reduce errors
  when CPU changes, but do not guarantee accurate ramps or unforeseen bursts.
  `forecastAggregation` applies to either strategy. Evaluate application
  outcomes and capacity on the intended workload before adopting this option.

Both tree models support `useUtilizationFeatures: false` inside their `xgboost`
or `lightgbm` configuration. This omits optional CPU usage/request lag features
while preserving the actual aggregate CPU history. Omitted or `true` retains
the existing feature set. Because utilization changes when capacity changes,
CPU-only features can reduce feedback from the scaler's own actions; accuracy
and capacity use still need workload validation. CPU requests and the configured
utilization target continue to convert forecasts into replicas. XGBoost's
experimental `survival:aft` objective requires utilization history and rejects
this option when false.

Use this block inside `spec.models` on an `HPAPlus` resource.

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

Use this block inside `spec.models` on an `HPAPlus` resource.

## Picking A Starting Point

Use this rule of thumb:

- Start with `Linear` if you want the simplest CPU-history predictive behavior.
- Use `HoltWinters` if the workload is strongly seasonal and interpretable season controls matter.
- Use `ARIMA` if you want a classical time-series model with explicit order control or incremental updates.
- Use `XGBoost` or `LightGBM` if you are tuning against recorded CPU history and want higher-capacity nonlinear models.

For the tree-based models in this repo, the practical workflow is:

1. Capture realistic runtime history.
2. Benchmark candidate parameter grids outside the cluster against that history.
3. Apply the chosen settings in the corresponding `HPAPlus` model block.

## Forecast runtime limits

ARIMA/SARIMA, XGBoost and LightGBM permit at most 10,000 forecast steps per request. For example, with one-second
samples, a horizon above 10,000 seconds is rejected; faster samples reach the limit at shorter horizons. The operator
does not silently shorten the forecast. Reduce `lookAhead` if logs report the step budget. Existing model failure,
backoff and fallback rules apply. Incremental ARIMA preserves its committed session when a faster incoming cadence
would exceed the limit. This fixed limit supplements execution timeouts; it is not a bound on training memory.

## Training history age retention

Optional `historyMaxAge` applies to each model's training window. For example, with `syncPeriod: 15000`:

```yaml
models:
  - name: demand
    type: Linear
    historyMaxAge: 600000
    linear:
      historySize: 30
      lookAhead: 60000
```

This retains up to 30 samples, each younger than ten minutes. A sample expires exactly when its age reaches
`historyMaxAge`. Omitted or null preserves existing count-only retention. This uses the recorded sampling slot;
source observation freshness remains controlled separately by `spec.metricMaxAge`.

Runtime validation requires `historyMaxAge >= retainedCount * roundedInterval`, where `roundedInterval` is
`max(1000, ceil(syncPeriod / 1000) * 1000)` milliseconds; omitted `syncPeriod` means 15000 ms. The retained count is
`historySize` (ARIMA defaults to 50), or `seasonalPeriods * storedSeasons` for Holt-Winters. For the example above the
minimum is 450000 ms. This conservative check accounts for one-second sampling and persisted slot precision.
Leave additional headroom for collection delays, missing observations and model execution: the minimum alone does
not guarantee prediction availability during outages or long fits. Invalid cross-field combinations are stored by the
API server but disable scaling until corrected, like the other controller-side constraints.

The sampler removes aged rows even while a model is waiting to start or CPU is unavailable. Reconciliation enforces
the same policy before prediction, so a delayed sampler cannot make expired training data usable. Pruning increments
the history epoch, clears cached forecasts, failure backoff and consumed progress, and retains the still-current rows.
The model warms up again if too few remain. Last-sampled slots and scheduled start times survive age pruning;
`resetDuration` still applies independently to inactivity. Pending scale journals and other model histories survive.

Fresh and cached forecasts expire when their oldest training input expires. This bound applies even when
`predictionMaxAge` is unset, and it can shorten opted-in cache reuse. Cache metadata retains the original training
start, so count pruning cannot extend reuse. Expiry is checked before/after fitting, during result and failure-backoff persistence retries,
and through the existing decision/journal/status write guards. Expired forecasts report `Expired` when available in the
current evaluation; insufficient retained rows report `WarmingUp`. The usual HPA fallback applies.

Physical cleanup occurs during sampling/reconciliation, not continuously. An already-sent API write cannot be revoked
atomically when a deadline passes. The next check enforces the window again. Age retention does not automatically
solve an oversized ConfigMap; the existing state/journal byte budgets also apply.

### Shared timed forecast decisions (opt-in)

A model can set `predictivePolicy: {scaleUpLeadSeconds: 30, tolerance: 0.1}`
with `predictionMaxAge` on Linear, ARIMA/SARIMA (one-shot or incremental),
Holt-Winters, LightGBM, and XGBoost (direct or recursive).
Each adapter returns its native predictions on a shared timestamped grid.
The worker returns timestamped aggregate CPU estimates instead of discarding
all but a scalar. The controller reevaluates the cached trajectory on every
reconciliation, independently of the model fitting cadence. The controller also
requeues at known trajectory boundaries; normal post-scale cooldown and rate
limits still apply.

Only the forecast interval covering the current time and increases within the
configured provisioning lead time affect the recommendation. Future decreases
cannot remove capacity before their timestamps. A new forecast replaces all
future points. An already-issued current interval can bridge the gap to its first
point, bounded by its original expiry; absent current data holds requested
capacity. Expired trajectories still follow the existing unavailable-model policy.
No reactive floor is implied by this feature.

Tolerance defaults to `0.1` (10%) when omitted or null inside `predictivePolicy`;
set `tolerance: 0` to disable it. Tolerance suppresses an upward change when forecast demand is no more than
`1 + tolerance` times the target capacity of the currently requested replicas.
With 250m requests, 60% target and 10% tolerance, an existing single replica can
hold a 151m forecast (up to 165m); 166m requests two. It does not change the target
or remove the one-pod minimum/rounding granularity. Normal min/max, rate limits,
partial-metric guards and configured stabilization still apply. Downscale stabilization
defaults to zero seconds. Set `behavior.scaleDown.stabilizationWindowSeconds` explicitly
to enable a window; an existing explicit value is preserved.

The policy supports arbitrary forecast horizons within a 32-point trajectory
budget, using the observed median sampling interval. For example, a five-minute
horizon at 30-second cadence produces ten points; lead time remains a separate
setting. Holt-Winters without explicit `lookAhead` returns one future interval
at the observed cadence. Linear evaluates its fitted line at every grid timestamp;
its scalar compatibility output still evaluates the exact configured horizon.
All models require distinct timestamped CPU history for timed output. The policy
does not apply the maximum of those ten points immediately.

For timed predictions, `predictionMaxAge` must exceed the sampling cadence.
A one-step horizon (for example 30 seconds at 30-second cadence) can use a
60-second maximum age so its predicted interval remains usable when that time
arrives. When maximum age exceeds the horizon, validity is capped by the end of
the final forecast interval as well as maximum age; it is never renewed merely
by reading the cache. Scalar forecasts retain their existing horizon ceiling.

`cpuDemandWeightExponent` optionally gives higher-CPU training targets more
influence in XGBoost's loss. It accepts a finite value from `0` to `2`; omission
or `0` keeps unweighted training. For a positive exponent `p`, each observed
training target `y` receives weight
`clip((max(y, 1) / max(median(targets), 1))^p, 0.5, 4)` (CPU in millicores).
Targets, timestamps and missing-data handling are unchanged. This still fits
one XGBoost model and does not introduce a reactive scaling recommendation.
Tune this option with application outcomes and allocated capacity: emphasizing
rare peaks can also increase overprediction. It is not enabled by default.

Experimental XGBoost CPU censoring is available with `objective: survival:aft`
and `forecastStrategy: direct`. It requires explicit
`aftLossDistributionScale` (0.05–2) and
`cpuCensoringUtilizationThreshold` (90–10000, percent of CPU requests).
Use this only when the configured threshold corresponds to known enforced CPU
capacity. For example, 90% can identify near-saturation when CPU limits equal
requests; 90% request utilization alone does not establish saturation for an
unlimited workload. The caller must establish that relationship.

Targets at or above that threshold are treated as lower bounds on demand by
XGBoost's native AFT objective. Other targets remain exact observations. Training
uses a reversible one-millicore offset because AFT requires positive labels;
predictions subtract that offset. Captured measurements are never overwritten.
This fits one model and adds no reactive recommendation. It requires aligned
utilization history and cannot be combined with `quantileAlpha`. The uncertainty
scale affects extrapolation and can cause substantial overprovisioning; this is
an opt-in experimental configuration, not a proven default.


## Shared forecast reliability and adaptation

`models[].forecastReliability` opts into shared monitoring of raw CPU forecast
errors. Omit it to preserve existing behavior. For example:

```yaml
forecastReliability:
  calibrationSamples: 20
  window: 5
  breaches: 4
  minimumRelativeError: 0.1
```

These are configurable initial defaults, not universal statistical guarantees.
After 20 matched predictions, the normal error bound is the greater of the
95th percentile of earlier absolute errors and `minimumRelativeError`.
Signed error is `(observedCPU - forecastCPU) / max(observedCPU, forecastCPU, 1)`;
CPU units are millicores. The one-millicore denominator floor handles genuine
zeros. Positive errors mean underprediction. Four of five recent errors must
exceed the learned bound in the same direction to trigger adaptation. The five
recent errors never train their own reference bound. Opposite-direction errors
and isolated spikes do not satisfy this rule. This detects deterioration relative
to earlier performance; it is not an absolute accuracy or SLO certification.

The runtime records one raw endpoint per forecast at a consistent horizon,
before replica rounding or cache merging. For trajectories it uses the final
point. Scalar forecasts require explicit forecast-time provenance; scalar
maximum aggregates without a trajectory are reported as unsupported. Direct
XGBoost can return its trajectory for monitoring without enabling timed scaling
or changing its configured scalar aggregation. New adapters can return an
explicit verification endpoint through the shared result contract.

Evaluation matches the complete, fresh observation's source timestamp to the
forecast endpoint's interval (one sample cadence, exclusive end). It does not
interpolate, fill missing samples with zeros, or score retrospective forecasts.
A source time and an overlapping forecast interval can each contribute at most
one error. A gap longer than twice the configured error-window length in sample
cadences clears recent evidence. This matches CPU samples by their reported
source timestamp; it is not an integration of the entire measurement window.
CPU saturation can hide offered demand, so this measures forecast accuracy for
observed CPU, not hypothetical unsatisfied demand.

Reliability state is persisted with model history and separated by workload UID,
target identity, configuration, history epoch, capacity and forecast horizon.
State is bounded to 128 pending endpoints, 100 calibration errors and 20 recent
errors. Overflow or missing measurements creates gaps. Changed identity or
horizon starts a new calibration; history reset/expiry clears reliability state.

A trigger marks `status.modelStatuses[].reliability.state` as `Degraded` and
requests adaptation on the next eligible fit with a new real training sample.
The optional adapter interface owns the response:

- Linear, XGBoost, LightGBM and Holt-Winters refit their configured rolling history.
  These adapters already fit on every normal execution; adaptation can advance a
  skipped model cadence but adds no new automatic window-shortening rule.
- Incremental ARIMA discards its fitted session and refits the configured history.
- Adapters without this capability continue ordinary forecasting and explicitly
  report `Unsupported`.

No adapter is forced onto twelve samples, and recorded training observations are
never rewritten. A failed adaptation uses the existing execution retry/backoff
policy. After a successful adaptation, old pending verification points are
cleared and new forecasts must demonstrate recovery: four of five within the
frozen normal error bound. Continued bad forecasts remain degraded rather than
repeatedly resetting on every reconciliation. Adapter completion alone does not
mark a model healthy.

Reliability is diagnostic and requests model work; it does not inject a reactive
recommendation, remove a scaling candidate, change `includeHPA`, or extend cache
expiry. Existing configured fallback behavior still applies if no forecast is
usable. Refitting the same rolling window may not cure a model mismatch; further
adapter strategies require separate validation.

The experimental `adaptiveSeasonality`, `seasonalPeriodHintSeconds`, automatic
cycle-shift/level-break detectors and twelve-sample override were removed. The
original optional `seasonalFeatures` still estimates a period from past data;
it no longer accepts a supplied period or shifts phase labels. The stronger
`retainIssuedIntervals` maximum-retention experiment was also removed.
`MergeCurrentInterval` remains: a valid old current interval bridges only the gap
before the first fresh interval, ending at its original expiry or the new
interval's start, whichever comes first. Fresh estimates replace overlapping
intervals, including lower estimates.


XGBoost can also forecast incoming request rates with live CPU-cost learning. See [request-demand forecasting](request-demand.md) for its opt-in configuration, telemetry requirements, and fallback behavior.
