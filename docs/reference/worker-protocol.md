# Forecast worker protocol

One-shot forecasts and the persistent ARIMA/SARIMA worker use version 1 JSON envelopes. Each response contains a
version, the aggregate CPU unit and the echoed request ID. The persistent worker exchanges newline-delimited
objects; the one-shot dispatcher accepts one request and exits after one response. Runtime limits and work budgets
are in [model runtime](model-runtime.md).

## Incremental envelope

Every request from the Go adapter includes:

| Field | Value |
| --- | --- |
| `protocolVersion` | Integer `1`. |
| `unit` | `cpu_millicores`; training values and forecasts represent aggregate CPU demand. |
| `requestId` | A new correlation ID for each exchange, echoed by the worker. Nonblank string, at most 128 characters. |
| `action` | `fit_forecast`, `append_forecast`, `forecast` or `reset`. |

`fit_forecast` supplies `config` and at least three `replicaHistory` entries. `append_forecast` supplies matching
config and newer entries; an empty append is allowed. Go sends history only in the action payload, never repeated
inside config. Forecast-only messages contain no history. `forecast` uses the fitted state and requires matching
config. `reset` clears the worker's state; the Go runner can also reset by ending and recreating the worker process.

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

An error response contains `ok: false` and an `error` string of at most 1024 characters. It echoes the protocol
metadata and a usable request ID when available. A reset acknowledgement contains `ok: true` without a prediction.

## Validation and state ownership

Versioned CPU samples must contain a nonnegative int64 `totalCpuUsageMillicores`; booleans, strings, fractions,
nulls and out-of-range values are rejected. Timestamps must be RFC3339 with an explicit timezone and at most nine
fractional digits. Samples must be strictly increasing, and appended samples must follow the last consumed sample.
Python keeps exact nanosecond ordering, including when validating appends; the Go side stores whole-second
timestamps. The worker orders history only against itself; the controller owns clock and freshness rules.

The header, the ARIMA configuration types and cross-field rules, and the history are validated before dispatch, so a
malformed request cannot reset or extend the fitted session. Versioned actions reject unknown top-level fields. Both
transports reject duplicate JSON keys and non-finite literals with the same decoder. Incremental frames have a 1 MiB
UTF-8 payload limit excluding LF/CRLF; an oversized frame is drained in bounded chunks and rejected, and parsing
continues with the next frame. A parse failure does not inherit the previous request ID.

Fit and append publish session state only after a valid forecast, so a failed forecast does not advance the consumed
history. The worker keeps at most as many samples as its last fit for timing and diagnostic metadata, and estimates
forecast steps from that recent window. Every appended observation still goes to statsmodels `extend`, which
continues the fitted state without a refit. Model or transport failures make Go reset the session and fall back.
Forecast responses contain int64 CPU millicores; negative forecasts are clamped to zero by the Go converter, and
replica conversion stays in Go.

Go requires a matching version, unit and request ID, an explicit boolean success status, and an integer prediction
on success. Unknown or duplicate response fields, trailing JSON, missing predictions, success with an error and int64
overflow are rejected. Field names are case-sensitive: `ProtocolVersion` is not an alias for `protocolVersion`. The
private Unix request and response envelope also rejects case aliases and explicit top-level null fields before
execution or reset; leave optional outer fields out instead of setting them to null. Null handling inside the model
input belongs to the model contract. A rejected worker response resets the session and falls back to a one-shot run
within the original model deadline. An expired deadline never gets a new fallback budget.

Requests without the version header get the unversioned request and response format. Go rejects an unversioned
response and can fall back to a one-shot run; it never reads an unversioned result as a versioned forecast.

## One-shot envelope

`algorithm.Python` implements `CPUForecastRunner`, and all five model adapters use it through `RunCPUForecast`. Each
call gets a fresh request ID and runs `algorithms/forecast.py` in the bounded process runner. For example:

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

`model` is one of `Linear`, `HoltWinters`, `ARIMA`, `XGBoost` or `LightGBM`. The dispatcher maps these fixed names
to bundled scripts and never accepts arbitrary paths. `input` holds the model-specific parameter object. Holt-Winters
receives `series`, paired increasing `timestamps` and an optional `lookAhead` in milliseconds. The timestamped models
share strict timezone-qualified RFC3339/RFC3339Nano parsing and exact nanosecond ordering. ARIMA/SARIMA and the trees
use the first estimated sampling step at or beyond `lookAhead`. Irregular spacing is accepted, as described in
[forecast policy](forecast-policy.md#timestamps-and-horizons).

The dispatcher rejects unknown or missing envelope fields, duplicate JSON keys, unsupported versions or units,
invalid request IDs, non-object inputs and non-finite JSON literals. It runs the selected script within the same
Python process. Model stdout is captured up to 4096 characters and must contain one signed decimal int64 forecast.
Partial output followed by an exception or an unsuccessful exit cannot become a successful forecast. Success uses the
same response as the incremental worker; failure returns `ok: false` with a bounded error and no prediction. Parse
failures may lack a request ID and are rejected by Go. Model stderr is subject to the parent runner's output limit.

The process input and output limits, queue, deadline and cancellation apply to the dispatcher. The whole encoded
request, envelope included, counts against the 1 MiB input limit. Transport failures still return runner errors. Go
validates both worker types with the same strict decoder and never downgrades a rejected one-shot reply to numeric
stdout. An incompatible incremental worker can fall back to this versioned one-shot path within its original
deadline.

Running a Python model script directly uses its plain command-line interface without envelopes. The operator always
uses the envelopes through `algorithm.Python`, so the image must contain the bundled dispatcher.

## Training input checks

The one-shot boundary rejects non-object samples and missing, negative, boolean, fractional, string or out-of-int64
CPU values before numerical libraries can coerce them. Observed replicas must be nonnegative int32 values.
Holt-Winters series use integer CPU values on the wire; fitting still uses the libraries' floating-point arithmetic.
All models require their minimum usable training window; command-line warm-up shortcuts do not apply to versioned
forecasts.

Timestamped samples need distinct timezone-qualified instants. Input may be unordered, because the adapters and
scripts sort it. Linear's explicit `currentTime` must equal the newest CPU sample. Optional auxiliary metric series
must align with history and contain finite nonnegative values, and so must per-sample utilization when supplied.
ARIMA one-shot calls may omit timestamps; the other models require them.

ARIMA configuration is checked for integer orders, horizons and seasonal periods, real boolean flags, supported
trends and information criteria, seasonal-field pairing and automatic-search bounds before coercion or any worker
change. Automatic search is limited to non-seasonal one-shot runs. Omitted values use defaults, and the optional
ARIMA trend, order limit and seasonal fields may be null; a null boolean is rejected. The Go adapter fills in
Kubernetes defaults before sending the configuration.

The trees need exact integers for depth, estimators, window size and leaf and sample counts. Numeric tuning values
must be finite numbers in their domain; strings and booleans are rejected. Optional null values use defaults, and an
explicit zero regularization is kept. Holt-Winters checks the smoothing numbers, a real boolean for damping, the
supported modes and the initialization fields before fitting. Known initialization needs finite level, trend and
seasonal values, and the other modes reject explicit initial values. Multiplicative modes need positive history.

## Optional diagnostics schema 1

One-shot and incremental version 1 requests may set `diagnosticsVersion: 1`. Other versions or types fail before
fitting or any session change. Successful replies then include `diagnostics` with the required `schemaVersion`,
`model`, `engine`, `engineVersion`, `action`, `windowSamples`, `forecastSteps`, `uncertainty`,
`trainingNanoseconds` and `inferenceNanoseconds`. Timestamped inputs also include exact RFC3339Nano `historyStart`,
`historyEnd` and `forecastAt`; Holt-Winters calls without timestamps leave those three out. Requests without the
option get the plain envelope.

Training and inference times are separate monotonic measurements, each capped at 24 hours in nanoseconds. Training
covers parameter search, fitting or incremental extension, and inference covers computing the forecast. Imports,
feature construction, transport and queue time are part of the whole-run duration instead. Forecast-only worker calls
have zero training time. For incremental workers, `windowSamples` and the history range describe the retained timing
window; the fitted state can carry earlier observations.

The engine is statsmodels, xgboost or lightgbm, and `engineVersion` names the installed distribution, including the
CPU-only XGBoost wheel. Actions are `one_shot`, `fit_forecast`, `append_forecast` and `forecast`. Uncertainty is
always `unavailable`. Provenance never contains feature values, credentials, paths or resource names. Failures with
diagnostics requested include a bounded `errorCode` (`invalid_input` or `model_failed`) and no prediction or
diagnostics. The code describes the failure class; a third-party engine's validation failure cannot always be told
apart from a configuration failure.

Go rejects unknown, duplicate, missing, null, out-of-range and mismatched provenance. The private Unix forecast
transport carries the same bounded diagnostics object next to the numeric value, and incremental exchanges keep
their JSON envelope. The runtime exposes the diagnostics on its result and logs them at debug level. Model status
stores whole-run timing and outcome; detailed engine provenance is not stored there. See
[uncertainty](forecast-policy.md#uncertainty).

The private Unix request asks for diagnostics with `diagnosticsVersion: 1` on the `forecast` operation. A client that
doesn't ask gets the three-field reply. A client that asks fails closed when diagnostics are missing or malformed.
Run the manager and the worker from the same image.
