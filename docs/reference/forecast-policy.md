# Forecast policy

This page covers when a forecast can be used: when it expires, how long it can be reused, how sample spacing becomes
forecast steps, and the optional timed decisions and reliability checks.

Forecasts are integer CPU demand in millicores, converted to replicas with the current conservative request
capacity. Zero demand is valid. Missing, malformed, mismatched, non-finite or overflowing output is unavailable, and
a failure never adds a zero candidate. All five models report versioned provenance, whether Python runs in the
manager or behind the worker socket. Provenance must match the model and the exact consumed timestamp, and requested
diagnostics cannot be left out.

## Expiry and reuse

Every forecast expires at the time it forecasts for, even without `predictionMaxAge`. Linear expires at its exact
horizon. The discrete models round the horizon up to whole sampling steps but expire no later than the requested
horizon. Holt-Winters without `lookAhead` expires at its one-step target and is also capped at `syncPeriod` for
reuse. A short horizon can therefore expire before a slow fit finishes; use a longer horizon or less training work.
Choose `lookAhead` longer than the age of the newest training sample plus the fitting time, because the sampler may
not have recorded its newest slot when a reconciliation starts.

`predictionMaxAge` lets a successful forecast be reused between fits for that many milliseconds after its newest
training sample, never past the forecast's own expiry. [`historyMaxAge`](#training-data-age) can end it earlier,
when the oldest training sample becomes too old. The earliest deadline applies to finishing the fit, publishing the
result, combining candidates, saving the scale journal and the final Scale and status writes. A deadline equal to
the current time counts as expired.

For example, with `perSyncPeriod: 2`, `predictionMaxAge: 45000` and `linear.lookAhead: 60000`, a successful forecast
can be used between fits for up to 45 seconds after its newest training sample, also after a later fit fails. A slow
fit doesn't get a new window when it finishes. Sampling continues independently, and reusing a cached forecast
doesn't mark new samples as consumed.

An expired forecast neither consumes history nor starts failure backoff; a failed fit can still start backoff.
Cached forecasts are tied to the model configuration, CPU request and target, workload identity and history epoch,
so changing any of them invalidates the cache. A reusable result is used only after its cache write succeeds. If a
candidate expires while the decision is being saved, the controller evaluates again before sending a scale change,
so an expired count never enters the stabilization history. An `Expired` model status keeps its consumed and expiry
times but has no usable replica count; a failed fit or backoff is reported first when present.

When no forecast is usable, HPA+ follows `predictionFailurePolicy`.

## Timestamps and horizons

The Python models accept timezone-qualified RFC3339 and RFC3339Nano timestamps and keep nanosecond ordering. Offsets
are normalized to the same instant, so time zones and daylight-saving changes don't change sample spacing. The
controller stores and sends training samples with whole-second precision.

Linear forecasts exactly `lookAhead` after the newest CPU sample. ARIMA/SARIMA (both modes), XGBoost and LightGBM
take the sampling interval as the median positive spacing between samples, at least 1 ms, and forecast
`ceil(lookAhead / interval)` steps. For example, a 15-second horizon with 10-second samples predicts two steps ahead.
Rounding the step count up doesn't extend reuse past `lookAhead`.

Holt-Winters takes an optional `holtWinters.lookAhead` and uses the same step rule; its Go adapter sends the
timestamps with the CPU series. Without `lookAhead` it forecasts one step, reuse stays capped at `syncPeriod`, and the
one-step target can shorten it further. With `lookAhead`, freshness is capped at `lookAhead` and reuse at
`min(predictionMaxAge, lookAhead)`.

Irregular sampling is accepted. The discrete models treat the retained values as an even grid at their median
interval, without interpolating, duplicating or filling missing values. Seasonal periods count samples, not wall
time, so long gaps can distort seasonality; `historyMaxAge` and `resetDuration` limit which samples stay in use.
Linear fits the real timestamps instead.

ARIMA one-shot calls without timestamps keep the input order and assume a one-second interval. The controller always
sends timestamps.

## Training data age

`historyMaxAge` limits how old a model's training samples may be. For example, with `syncPeriod: 15000`:

```yaml
models:
  - name: demand
    type: Linear
    historyMaxAge: 600000
    linear:
      historySize: 30
      lookAhead: 60000
```

This keeps up to 30 samples, each younger than ten minutes. A sample expires exactly when its age reaches
`historyMaxAge`. Without it, only the count limits the window. Age is measured from the sampling slot; the age of the
source measurement is limited separately by [`metricMaxAge`](configuration.md#metricmaxage).

Validation requires `historyMaxAge >= retainedCount * roundedInterval`, where `roundedInterval` is
`max(1000, ceil(syncPeriod / 1000) * 1000)` milliseconds and an omitted `syncPeriod` means 15000. The retained count
is `historySize` (ARIMA defaults to 50), or `seasonalPeriods * storedSeasons` for Holt-Winters. For the example
above the minimum is 450000 ms. This check allows for one-second sampling and the precision of stored slots. Leave
extra room for collection delays, missing samples and fitting time: the minimum alone doesn't keep forecasts
available through outages or long fits. An invalid combination is stored by the API server but stops scaling until
it is fixed, like the other controller-side checks.

The sampler removes aged rows even while a model is waiting to start or CPU is unavailable, and reconciliation
applies the same limit before forecasting, so a delayed sampler cannot make expired data usable. Removing rows
increments the history epoch, clears cached forecasts, failure backoff and consumed progress, and keeps the rows
that are still current. The model warms up again if too few remain. Sample slots and scheduled start times are kept,
`resetDuration` still applies to inactivity, and pending scale journals and other models' histories are not touched.

Forecasts, fresh or cached, expire when their oldest training sample expires, even without `predictionMaxAge`. The
cache remembers the original start of its training window, so removing rows by count cannot extend reuse. Expiry is
checked before and after fitting, while saving results and backoff, and by the decision, journal and status write
checks. An expired forecast reports `Expired`, and a model with too few remaining rows reports `WarmingUp`.

Old rows are removed during sampling and reconciliation, not continuously. An API write that was already sent cannot
be taken back when a deadline passes; the next check applies the window again. Age limits don't shrink an oversized
ConfigMap; the [storage budgets](history-storage.md#storage-layout) still apply.

## Timed decisions

A model can set `predictivePolicy` together with `predictionMaxAge`:

```yaml
predictivePolicy:
  scaleUpLeadSeconds: 30
  tolerance: 0.1
```

Linear, ARIMA/SARIMA (one-shot or incremental), Holt-Winters, LightGBM and XGBoost (direct or recursive) support it.
Each adapter returns its predictions on a shared timestamped grid, and the worker returns timestamped CPU estimates
instead of a single number. The controller evaluates the cached trajectory again on every reconciliation,
independently of how often the model fits, and also requeues at known trajectory boundaries. The usual post-scale
cooldown and rate limits apply.

Only the interval covering the current time, and increases within the scale-up lead time, affect the
recommendation. A predicted decrease cannot remove capacity before its time. A new forecast replaces all future
points. An interval already issued for the current time can bridge the gap until the new forecast's first interval,
ending at its own expiry or at the start of the new interval, whichever comes first; fresh estimates replace
overlapping intervals, including lower ones. Without current data, the requested capacity is held. An expired
trajectory is handled like any unavailable forecast. No reactive floor is added.

`tolerance` defaults to `0.1` (10%) when omitted or null; `tolerance: 0` turns it off. It skips an upward change
while forecast demand is at most `1 + tolerance` times the target capacity of the currently requested replicas. With
250m requests, a 60% target and 10% tolerance, one replica holds a 151m forecast (up to 165m), and 166m needs two.
Tolerance doesn't change the target or the one-pod rounding. Min/max, rate limits, partial-metric guards and
stabilization still apply. Scale-down stabilization defaults to zero seconds; set
`behavior.scaleDown.stabilizationWindowSeconds` to use a window.

The trajectory can cover any horizon within a 32-point budget, at the observed median sampling interval. For
example, a five-minute horizon at a 30-second cadence gives ten points; the lead time is a separate setting.
Holt-Winters without `lookAhead` returns one interval at the observed cadence. Linear evaluates its fitted line at
every grid time, and its single-value output still uses the exact configured horizon. All models need distinct
timestamped CPU samples for timed output. The policy does not apply the maximum of the ten points at once.

For timed forecasts, `predictionMaxAge` must be longer than the sampling cadence. A one-step horizon, for example
30 seconds at a 30-second cadence, can use a 60-second maximum age so that its interval is still usable when it
arrives. When the maximum age is longer than the horizon, validity also ends with the last forecast interval;
reading the cache never renews it. Single-value forecasts keep their horizon limit.

With [request demand](../user-guide/request-demand.md#lead-time-from-pod-startup), `scaleUpLeadFromStartup: true`
derives the lead time from how fast the workload's pods actually start.

## Reliability checks

`forecastReliability` turns on tracking of raw CPU forecast errors for a model. It is off unless set:

```yaml
forecastReliability:
  calibrationSamples: 20
  window: 5
  breaches: 4
  minimumRelativeError: 0.1
```

These are starting values, not statistical guarantees. After 20 matched forecasts, the normal error bound is the
greater of the 95th percentile of earlier absolute errors and `minimumRelativeError`. The signed error is
`(observedCPU - forecastCPU) / max(observedCPU, forecastCPU, 1)` with CPU in millicores; the one-millicore floor
handles real zeros, and positive errors mean underprediction. Four of the five most recent errors must exceed the
bound in the same direction to trigger adaptation, and those recent errors never train their own bound.
Opposite-direction errors and isolated spikes don't count. This detects a model getting worse than it was; it is not
an absolute accuracy or SLO check.

The runtime records one raw endpoint per forecast at a consistent horizon, before replica rounding or cache
merging. For trajectories it uses the final point. Single-value forecasts need explicit forecast-time provenance,
and a `maximum` aggregation without a trajectory is reported as unsupported. Direct XGBoost can return its trajectory
for monitoring without turning on timed scaling or changing its aggregation. Adapters can return an explicit
verification endpoint through the shared result contract.

Each forecast endpoint is matched with the complete, fresh observation whose source time falls in the endpoint's
interval (one sample cadence, end excluded). Nothing is interpolated, missing samples are not filled with zeros,
and old forecasts are not scored after the fact. A source time and an overlapping forecast interval can each give at
most one error. A gap longer than twice the error window, counted in sample cadences, clears the recent errors. CPU
saturation can hide demand, so this measures accuracy against observed CPU, not against unserved demand.

Reliability state is stored with the model history and separated by workload UID, target identity, configuration,
history epoch, capacity and horizon. It holds at most 128 pending endpoints, 100 calibration errors and 20 recent
errors; overflow and missing measurements create gaps. A changed identity or horizon starts a new calibration, and a
history reset or expiry clears the state.

A trigger sets `status.modelStatuses[].reliability.state` to `Degraded` and asks for adaptation on the next eligible
fit with a new real training sample. Each adapter decides what adaptation means:

- Linear, XGBoost, LightGBM and Holt-Winters refit their configured rolling history. They fit on every run anyway, so
  adaptation can only make a model run before its normal cadence; it doesn't shorten the window.
- Incremental ARIMA discards its fitted session and refits the configured history.
- Adapters without this capability keep forecasting and report `Unsupported`.

Recorded training samples are never rewritten. A failed adaptation uses the normal retry and backoff. After a
successful adaptation, old pending endpoints are cleared and new forecasts must show recovery: four of five within
the frozen normal bound. A model that keeps forecasting badly stays degraded instead of resetting on every
reconciliation, and finishing an adaptation alone does not mark a model healthy.

Reliability is diagnostic and only asks for model work. It never adds a reactive recommendation, removes a
candidate, changes `includeHPA` or extends cache expiry. Refitting the same rolling window may not fix a model that
doesn't suit the workload.

## Uncertainty

Forecasts are **point forecasts without uncertainty**. Provenance reports `uncertainty: unavailable`. HPA+ doesn't
treat agreement between models, fit success, training time or the engines' own standard errors as calibrated
confidence. Every eligible model is one candidate in the configured combination; there is no confidence weighting
or upper-bound selection.

Use observe mode to check a model before giving it control. Assist mode stops predictive scale-down below the
baseline. Min/max replicas, stabilization and the HPA behavior rate limits bound forecast-driven changes after the
candidates are combined, for fresh and cached forecasts alike. HPA+ has no calibrated prediction intervals.
