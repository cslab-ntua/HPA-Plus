# Forecast validity and uncertainty policy

Production forecasts are integer CPU demand in millicores, converted to replicas with the current conservative request
capacity. Zero demand is valid; missing, malformed, mismatched, nonfinite or overflowing output is unavailable. Failures
never add a zero candidate. All five engines provide versioned provenance through direct and private Unix execution.

## Freshness

A fresh production forecast expires at its data-relative target, even when `predictionMaxAge` is omitted. Linear uses
its exact requested horizon; discrete models use median-spacing steps rounded upward, with the requested horizon as a
conservative freshness ceiling. Holt with omitted `lookAhead` expires at its actual one-step target. This can exclude a
short-horizon forecast whose training finishes too late; increase the horizon or reduce training work in that case.

`predictionMaxAge` enables reuse and may shorten validity further. `historyMaxAge` limits the oldest training input.
The earliest applicable deadline governs completion, conditional result publication, ensemble evaluation, journal
publication and the final Scale/status write. Equality is expired. An expired successful forecast neither consumes history nor
opens failure backoff. A failed fit can still open retry backoff; it has no successful target metadata to publish. Cached values remain fingerprinted by configuration, capacity, identity and history epoch.

Provenance must match the model and the exact consumed timestamp. Requested diagnostics cannot be silently omitted.
The compatibility seam for injected legacy numeric runners retains its old behavior; it is not the production Python
path. No new admission fields or defaults are introduced.

## Uncertainty and change limits

The supported policy is **point forecasts with explicitly unavailable uncertainty**. Engine provenance reports
`uncertainty: unavailable`; the operator does not treat agreement, fit success, training duration or engine-native
standard errors as calibrated confidence. Every eligible model contributes to the configured ensemble with its existing
semantics; unsupported confidence weighting or upper-bound selection is not silently approximated.

Use observe mode to validate a model before granting scaling authority. Assist mode constrains predictive downscaling
against the baseline. Existing min/max replicas, stabilization and per-direction HPA behavior rate/disabled rules bound
forecast-driven changes after ensemble selection. These bounds apply equally to fresh and cached forecasts.

Calibrated intervals and conservative upper-bound selection are optional future extensions, requiring held-out
coverage evidence for each family and explicit admission/transport policy. This defines the supported uncertainty policy rather than claiming calibrated prediction intervals.
