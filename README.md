# HPA+

HPA+ is a Kubernetes predictive autoscaling operator. It keeps the familiar Horizontal Pod Autoscaler configuration and adds a modular suite of models that forecast aggregate CPU demand. The operator converts those forecasts into replica recommendations using CPU requests and the configured utilization target.

Supported models are Linear, Holt-Winters, ARIMA/SARIMA, XGBoost and LightGBM. Models share history collection, workload identity checks, forecast validity checks, worker limits and the scaling decision path. Predictive performance depends on the workload, measurements and configuration.

Documentation: https://cslab-ntua.github.io/HPA-Plus/

## Results

In a 4-hour test on a live cluster, HPA+ with [request-demand forecasting](docs/user-guide/request-demand.md) answered 99.9% of requests within 500 ms during repeated traffic peaks. HPA+ forecasting from CPU alone answered 93.6%, and the reactive HPA formula with the same scaling limits answered 28.5%. The [benchmark results](docs/results/README.md) describe the setup, every number and the limits.

![Headline numbers from the 4-hour benchmark run](docs/results/figures/overview.svg)

## How it works

1. Collect workload metrics and retain complete CPU observations in durable history.
2. Compute the reactive HPA-style recommendation and evaluate eligible models.
3. Convert forecast CPU demand into replicas, optionally combining it with the reactive recommendation through `includeHPA`.
4. Apply the configured decision policy, rollout mode, replica bounds and scaling behavior.
5. Update the workload's Scale resource and publish status, logs and operational metrics.

Sampling and reconciliation run independently. Missing CPU remains missing; forecasts have source timestamps and expiry. Timed predictive policies can preserve the forecast covering the current interval and defer future decreases until their interval begins.

## Build and install

The operator requires Kubernetes `autoscaling/v2` support and a working resource metrics API for CPU forecasting. The supplied image targets Linux/amd64. Use an image tag that identifies your actual checkout:

```bash
export REGISTRY=your-registry
export VERSION=your-build-tag
make docker REGISTRY="$REGISTRY" VERSION="$VERSION"
make push REGISTRY="$REGISTRY" VERSION="$VERSION"
helm upgrade --install hpa-plus-operator ./helm \
  --namespace hpa-plus-system --create-namespace \
  --set image.repository="$REGISTRY/hpa-plus-operator" \
  --set image.tag="$VERSION"
```

The Helm chart installs the CRD, operator and required permissions. Each `HPAPlus` belongs in the same namespace as its target workload. Start new configurations in `observe` mode and inspect their status before enabling control. Only one autoscaler should write a workload's replica count.

See [installation](docs/user-guide/installation.md), [getting started](docs/user-guide/getting-started.md), [configuration](docs/reference/configuration.md) and [models](docs/user-guide/models.md).

To watch what HPA+ decides and why, run `make ui` and open the address it prints. The [UI guide](docs/user-guide/ui.md) explains the read-only page.

## Development

Use the Go version declared in `go.mod` and the Python runtime/dependencies pinned by `Dockerfile` and `algorithms/requirements.txt`.

- `make build`: build the manager, the worker server and the cgroup collector.
- `make py_dependencies`: install the pinned model, docs and dev dependencies.
- `make docs`: build the docs site with the same checks as CI.
- `make generate`: regenerate deepcopy code, the CRD and operator RBAC.
- `make docker` / `make push`: build or publish an operator image.
- `make ui`: serve the read-only local UI on http://hpaplus.localtest.me:8080.

`pre-commit install` adds hooks that run whitespace checks, gofmt, go vet and go test before each commit.

The [architecture guide](docs/architecture.md) describes the controller, forecasting interface, history and worker boundaries. [Operations](docs/user-guide/operations.md) covers status, resources, logs and recovery.

## Attribution and license

HPA+ extends [Predictive Horizontal Pod Autoscaler](https://github.com/jthomperoo/predictive-horizontal-pod-autoscaler), created and maintained by Jamie Thompson (`jthomperoo`) and its upstream contributors. Their work and attribution are preserved under the [Apache License 2.0](LICENSE).
