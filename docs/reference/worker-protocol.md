# Forecast worker protocol

Production one-shot forecasts and the persistent ARIMA/SARIMA worker use version 1 JSON envelopes. Each response
contains a version, aggregate CPU unit and echoed request ID. The persistent worker exchanges newline-delimited objects;
the one-shot dispatcher accepts one request and exits after one response.

## Incremental envelope

Every request from the Go adapter includes:

| Field | Value |
| --- | --- |
| `protocolVersion` | Integer `1`. |
| `unit` | `cpu_millicores`; training values and forecasts represent aggregate CPU demand. |
| `requestId` | A new correlation ID for each exchange, echoed by the worker. Nonblank string, at most 128 characters. |
| `action` | `fit_forecast`, `append_forecast`, `forecast` or `reset`. |

`fit_forecast` supplies `config` and at least three `replicaHistory` entries. `append_forecast` supplies matching config
and newer entries; an empty append is allowed. Go sends history only in the action payload, without repeating the full
training window inside config. Forecast-only messages contain no history. `forecast` uses the fitted state and requires matching config. `reset`
clears the worker's state. The Go runner also resets by terminating and recreating the worker process.

Example request, formatted across lines for readability:

```json
{
  "protocolVersion": 1,
  "unit": "cpu_millicores",
  "requestId": "example",
  "action": "fit_forecast",
  "config": {"order": [1, 0, 0], "lookAhead": 1000},
  "replicaHistory": [
    {"time": "2026-09-20T00:00:00Z", "totalCpuUsageMillicores": 100},
    {"time": "2026-09-20T00:00:01Z", "totalCpuUsageMillicores": 100},
    {"time": "2026-09-20T00:00:02Z", "totalCpuUsageMillicores": 100}
  ]
}
```

Successful forecast response:

```json
{"protocolVersion":1,"unit":"cpu_millicores","requestId":"example","ok":true,"prediction":100}
```

An error response contains `ok: false` and an `error` string limited to 1024 characters. It echoes supported protocol
metadata and a usable request ID when available. A reset acknowledgement contains `ok: true` without a prediction.

## Validation and state ownership

Versioned CPU samples must contain nonnegative int64 `totalCpuUsageMillicores`; booleans, strings, fractions, nulls and
out-of-range values are rejected. Timestamps require RFC3339 with an explicit timezone and at most nine fractional digits.
Samples must be strictly increasing; appended samples must follow the last consumed sample. Exact decimal timestamps retain nanosecond ordering in Python, including incremental append validation. Go's current
persisted history uses whole-second timestamps. The worker orders history against itself; the controller owns clock/freshness policies.

Header, ARIMA configuration types/cross-field rules and history validation run before dispatch, so malformed requests
cannot reset or extend the fitted session. Versioned actions reject unknown top-level fields. Both transports reject
duplicate JSON keys and non-finite literals using the same decoder. Incremental frames have a 1 MiB UTF-8 payload
limit excluding LF/CRLF; oversized frames are drained in bounded chunks, rejected, and followed by normal parsing
of the next frame. Parse failures do not inherit the previous request ID.
Fit and append publish session state only after a valid forecast, so failed forecasts do not advance the consumed
history. The worker retains at most the number of samples supplied to its last fit for timing/diagnostic metadata;
forecast step estimation uses that recent window. Every appended observation still reaches statsmodels `extend`, which
continues the fitted state without a refit. Model execution/transport failures still trigger Go session reset and fallback.
Forecast responses contain int64 CPU millicores; negative
forecasts retain the Go converter's zero-clamp policy. Replica capacity conversion remains in Go.

Go requires a matching version, unit and request ID, an explicit boolean success status, and an integer prediction on
success. Unknown or duplicate response fields, trailing JSON, missing predictions, success-with-error and int64 overflow are rejected.
Go response and diagnostic field names are case-sensitive: `ProtocolVersion` is not an alias for `protocolVersion`.
The private Unix request/response envelope also rejects case aliases and explicit top-level null fields before
execution or reset. Omit optional outer fields instead of setting them to null; model-input null handling remains
owned by the model contract. Canonical version-1 clients retain the same wire format.
A rejected worker response resets the session and follows the existing one-shot fallback within the original model
deadline. An expired deadline never receives a new fallback budget.

## Compatibility

- New Go and new worker use the versioned exchange.
- Headerless requests from older clients retain the worker's legacy request/response behavior.
- New Go rejects an old worker's headerless response and can use one-shot fallback. It does not silently interpret an
  unversioned worker result as a versioned forecast.

## One-shot envelope

`algorithm.Python` implements `CPUForecastRunner`; all five model adapters select that capability through
`RunCPUForecast`. Each call gets a fresh request ID and invokes `algorithms/forecast.py` in the existing bounded process
runner. For example:

```json
{
  "protocolVersion": 1,
  "unit": "cpu_millicores",
  "requestId": "example-one-shot",
  "model": "Linear",
  "input": {
    "lookAhead": 1000,
    "currentTime": "2020-01-01T00:00:01Z",
    "replicaHistory": [
      {"time": "2020-01-01T00:00:00Z", "replicas": 1, "totalCpuUsageMillicores": 100},
      {"time": "2020-01-01T00:00:01Z", "replicas": 1, "totalCpuUsageMillicores": 200}
    ]
  }
}
```

`model` is one of `Linear`, `HoltWinters`, `ARIMA`, `XGBoost`, or `LightGBM`. The dispatcher maps these fixed identifiers
to bundled scripts; it does not accept arbitrary paths. `input` holds the existing model-specific parameter object.
Holt-Winters receives `series`, paired increasing `timestamps`, and optional `lookAhead` in milliseconds. Timestamped models share strict timezone-qualified RFC3339/RFC3339Nano parsing and exact nanosecond ordering.
ARIMA/SARIMA and trees use the first estimated sampling step at or beyond lookAhead. Comprehensive malformed-field
validation remains layered; the [documented spacing policy](../user-guide/models.md) accepts irregular data without interpolation.

The dispatcher rejects unknown/missing envelope fields, duplicate JSON keys, unsupported versions/units, invalid request
IDs, non-object inputs and non-finite JSON literals. It runs the selected script within the same Python process. Model
stdout is captured up to 4096 characters and must contain one signed decimal int64 forecast. Partial output followed by
an exception or unsuccessful exit cannot become a successful forecast. Success uses the same response as incremental
workers; failure returns `ok: false` with a bounded error and no prediction. Parse failures may lack a request ID and
are rejected by Go. Model stderr remains subject to the parent runner's output limit.

The existing input/output process budgets, queue, parent deadline and cancellation apply to the dispatcher. The entire
encoded request, including its envelope, counts against the 1 MiB input limit. Transport failures still return runner
errors. Go validates both worker types through the same strict decoder and never downgrades a rejected one-shot reply
to numeric stdout. An incompatible incremental worker can fall back to this versioned one-shot path within its original
deadline.

Direct invocation of individual Python model scripts retains the legacy interface. Injected Go runners without
`CPUForecastRunner` retain their existing numeric interface; production wiring uses `algorithm.Python` and therefore
always uses envelopes. New operator binaries require the bundled dispatcher in the runtime image.

Tests cover all five dispatch routes, metadata and numeric boundaries, malformed output, bounded errors, stream
restoration, cancellation and legacy runner injection. Real pinned-runtime contracts cover successful models, invalid
model parameters and incremental-to-one-shot fallback.

## Versioned training input checks

The one-shot boundary rejects non-object samples and missing, negative, boolean, fractional, string or out-of-int64 CPU
values before numerical libraries can coerce them. Observed replicas must be nonnegative int32 values. Holt-Winters
series use integer CPU values on the Go/Python wire; numerical fitting still uses the libraries' floating-point arithmetic.
All models require their minimum usable training window. Legacy CLI warmup fallbacks cannot satisfy a versioned forecast.

Timestamped samples require distinct timezone-qualified instants; input may be unordered because adapters/scripts sort
it. Linear's explicit currentTime must equal the latest CPU sample. Optional auxiliary metric series must align with
history and contain finite nonnegative values. Per-sample utilization, when supplied, has the same numeric rule.
Legacy ARIMA one-shot missing-timestamp behavior is preserved; other versioned timestamped models require timestamps.

Versioned ARIMA configuration validates integer orders/horizons/seasonal periods, actual boolean flags, supported trends
and information criteria, seasonal-field pairing and automatic-search bounds before coercion or worker mutation.
Auto-search is limited to non-seasonal one-shot execution. Omitted values retain defaults; optional ARIMA trend/order
limit/seasonal fields may be null. A null boolean is rejected. Kubernetes optional null/default behavior is unchanged
because the Go adapter resolves those defaults before serializing the worker configuration.

Trees require exact integers for depth, estimators, window size and leaf/sample counts. Numeric tuning must be finite
numbers within its domain; strings and booleans are rejected. Optional null values retain defaults, and explicit zero
regularization is preserved. Holt-Winters validates smoothing numbers, actual boolean damping, supported modes and
initialization fields before fitting. Known initialization requires finite level/trend/seasonal values; the other modes
reject explicit initial values. Multiplicative modes require positive history.

Configuration validity is distinct from runtime work limits. See the model guide for spacing semantics and
[model runtime](model-runtime.md) for resource controls and measurements; model quality remains separate work.

## Discrete forecast work budget

ARIMA/SARIMA, XGBoost, LightGBM and Holt-Winters with explicit lookAhead reject requests requiring more than 10,000 future sampling steps. The shared
interval uses exact median positive spacing (minimum 1ms); horizons round upward. This is a fixed runtime limit; it does not clamp the
forecast horizon or replace the process timeout. Shorten `lookAhead` when the observed sampling interval makes the
requested step count excessive. Legacy missing-timestamp ARIMA uses its usual one-second interval fallback.

Versioned one-shot validation rejects excessive horizons before model execution. Direct entrypoints also check before
feature allocation, parameter search or fitting. Workers check fits before fitting, and append requests against their
prospective retained timestamps before extending. A cadence change can therefore reject an append even when
configuration is unchanged; the previous committed session remains available. Forecast-only calls are checked too.
Failure responses contain no numeric prediction. Linear evaluates one future point. Holt uses the same discrete cap for explicit horizons; omitted horizons retain one step. Training and search limits apply separately below.


## Training and search work budgets

Shared Python checks reject work before feature allocation, numerical model construction or incremental extension.
The versioned dispatcher checks before loading the model implementation; legacy CLIs and the persistent worker also
check their execution paths. Rejections contain `training budget exceeded`, return no forecast and preserve committed
worker state. Existing Go failure/backoff/fallback behavior applies. Configurations are not silently reduced.

| Work proxy | Maximum |
| --- | --- |
| History rows per fit or incremental append batch, all models | 10,000 |
| Tree matrix cells | 1,000,000 |
| Tree estimators | 2,000 |
| Tree depth | 16; LightGBM still accepts -1 with its leaf cap |
| LightGBM leaves | 65,536 |
| Tree matrix cells × estimators | 200,000,000 |
| Automatic ARIMA fits, including baseline/grid and final selected fit | 256 |
| ARIMA state dimension proxy | 128 |
| ARIMA history rows × state dimension squared | 2,000,000 |

For rolling window `w`, CPU feature width is `w + max(1,w-1) + 2`; auxiliary metrics add `w+2`.
Matrix cells use `max(1, historyRows-lags) × width`, reserving an inference row even for legacy warmup calls.
The ARIMA state proxy is `max(p+s*P, q+s*Q+1) + d+s*D`, where `s` is the seasonal period. Automatic
search checks its largest orders and `(maxP+1)*(maxD+1)*(maxQ+1)+1` fits before entering the search.
Default parameters remain unchanged. These fixed implementation limits add no CRD tuning fields.

These are conservative workload proxies, not strict RSS/CPU-second limits or performance guarantees. Libraries may
allocate several arrays, optimizer workspaces or trees beyond the counted cells. Process deadlines, bounded concurrency
and container memory limits remain necessary. Benchmarking, configurable per-model budgets and runtime isolation are
still open work.


### Process isolation

Both Go runners enter `algorithms/worker_limits.py` before numerical imports. The default hard RLIMIT_AS is
1024 MiB per process; child numerical-library thread settings are forced to one. The wrapper preserves stricter
inherited hard limits. Memory allocation failure is a failed execution and triggers existing reset/backoff/fallback
handling. See [runtime controls and measurements](model-runtime.md). A per-process address-space ceiling is not a
shared RSS limit; the Kubernetes pod limit and configured concurrency/session counts still determine aggregate risk.

## Optional diagnostics schema 1

One-shot and incremental v1 requests may supply `diagnosticsVersion: 1`. Other versions/types fail before fitting or
session mutation. Successful forecast replies then include `diagnostics` with required `schemaVersion`, `model`, `engine`,
`engineVersion`, `action`, `windowSamples`, `forecastSteps`, `uncertainty`, `trainingNanoseconds` and
`inferenceNanoseconds`. Timestamped inputs also include exact RFC3339Nano `historyStart`, `historyEnd` and `forecastAt`.
Legacy Holt series-only calls omit the timestamp trio. Requests without the option retain the original envelope.

Training and inference durations use independent monotonic measurements, each capped at 24 hours in nanoseconds.
Training includes parameter search, fitting or incremental extension; inference includes forecast computation.
Imports, feature construction, transport and queue time remain in the existing whole-run duration, not these stage
measurements. Forecast-only worker calls have zero training time. Incremental `windowSamples` and history range describe
the bounded retained timing window; the fitted state can carry earlier observations beyond that window.

The engine is statsmodels, xgboost or lightgbm; engineVersion identifies the installed distribution, including the
CPU-only XGBoost wheel. Actions are `one_shot`, `fit_forecast`, `append_forecast` and `forecast`. Uncertainty is explicitly
`unavailable`. No feature values, credentials, paths or resource names appear in provenance. Opt-in failures include
bounded `errorCode` (`invalid_input` or `model_failed`) and no prediction/diagnostics. Codes describe failure class,
not a promise that a third-party engine's validation failures can always be distinguished from configuration failures.

Go rejects unknown, duplicate, missing, null, invalid-range and mismatched provenance. The private Unix forecast
transport carries the same bounded diagnostics object alongside its existing numeric value; incremental exchanges
retain their JSON envelope. The runtime exposes caller-owned diagnostics on its result and logs them at debug level.
Existing bounded model status stores whole-run timing/outcome; detailed engine provenance is not persisted there.
See [validity and uncertainty policy](forecast-policy.md).

The private Unix request also opts in with `diagnosticsVersion: 1` on the `forecast` operation. Original clients receive
the original three-field transport reply. New clients fail closed if requested diagnostics are absent or malformed;
there is no silent metadata downgrade. Manager and worker should run the same release image.
