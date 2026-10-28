# Hooks

A hook lets HPA+ ask your own service for values while it runs. HPA+ has one hook: Holt-Winters
`runtimeTuningFetchHook`, which fetches the `alpha`, `beta` and `gamma` smoothing factors from an HTTP service before
each fit.

## Allow the service first

Hooks are disabled until the operator administrator allows the service's origin in the Helm values:

```yaml
hooks:
  allowedOrigins:
    - https://tuning.example.com
```

The manager flag is `--hook-allowed-origins`. Without an allowed origin every hook request fails with
`hook destination is not allowed`, and the model reports a failed fit. [Hook security](../reference/hook-security.md)
explains the origin rules, the blocked addresses and bearer tokens from Secrets.

## Example

```yaml
models:
  - type: HoltWinters
    name: seasonal
    holtWinters:
      seasonalPeriods: 6
      storedSeasons: 4
      trend: additive
      seasonal: additive
      runtimeTuningFetchHook:
        type: http
        timeout: 2500
        http:
          method: GET
          url: https://tuning.example.com/holt-winters
          successCodes:
            - 200
          parameterMode: query
```

- `type`: always `http`.
- `timeout`: the longest the request may take, in milliseconds. A slower request counts as a failure.
- `http.method`: the HTTP method, for example `GET` or `POST`.
- `http.url`: the service URL. Its origin must be in `hooks.allowedOrigins`. URLs with credentials are rejected, and
  redirects are not followed.
- `http.successCodes`: the status codes that count as success. Any other status is a failure.
- `http.headers`: optional extra headers. They are stored in the HPAPlus object, so don't put secrets in them; use
  [hook credentials](../reference/hook-security.md#secret-based-bearer-authentication) instead.
- `http.parameterMode`: `query` sends the request data in the `value` query parameter, `body` sends it as a JSON
  request body.

The same hook with `POST` and a body:

```yaml
runtimeTuningFetchHook:
  type: http
  timeout: 2500
  http:
    method: POST
    url: https://tuning.example.com/holt-winters
    successCodes:
      - 200
      - 202
    parameterMode: body
```

## Request and response

HPA+ sends the model configuration, without its hook settings, and the training history. Shortened:

```json
{
  "model": {
    "type": "HoltWinters",
    "name": "seasonal",
    "holtWinters": {"seasonalPeriods": 6, "storedSeasons": 4, "trend": "additive", "seasonal": "additive"}
  },
  "replicaHistory": [
    {"time": "2026-10-20T10:00:00Z", "replicas": 3, "totalCpuUsageMillicores": 420},
    {"time": "2026-10-20T10:00:15Z", "replicas": 3, "totalCpuUsageMillicores": 465}
  ]
}
```

The service answers with the factors to use:

```json
{"alpha": 0.9, "beta": 0.3, "gamma": 0.5}
```

A factor left out of the response falls back to the value in `holtWinters`. If a factor is missing from both, or the
request fails, the fit fails and the model follows the normal retry and fallback rules. Responses are limited to
64 KiB.
