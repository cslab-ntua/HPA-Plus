[![Build](https://github.com/cslab-ntua/HPA-Plus/workflows/main/badge.svg)](https://github.com/cslab-ntua/HPA-Plus/actions)
[![go.dev](https://img.shields.io/badge/go.dev-reference-007d9c?logo=go&logoColor=white&style=flat)](https://pkg.go.dev/github.com/cslab-ntua/HPA-Plus)
[![Go Report Card](https://goreportcard.com/badge/github.com/cslab-ntua/HPA-Plus)](https://goreportcard.com/report/github.com/cslab-ntua/HPA-Plus)
[![Docs](https://img.shields.io/badge/docs-GitHub-blue)](https://github.com/cslab-ntua/HPA-Plus/tree/master/docs)
[![License](https://img.shields.io/:license-apache-blue.svg)](https://www.apache.org/licenses/LICENSE-2.0.html)

# HPA+

HPA+ is a benchmark-first predictive autoscaling operator for Kubernetes.

The point of this repository is not just “run a forecasting model next to HPA.” It is to keep the HPA control shape,
capture runtime history, benchmark competing predictive strategies against that history, and then ship the chosen model
back into the running autoscaler.

## Project lineage and attribution

HPA+ is an extension of the original Predictive Horizontal Pod Autoscaler (PHPA).

The upstream project is maintained at:
<https://github.com/jthomperoo/predictive-horizontal-pod-autoscaler>

We explicitly acknowledge and credit Jamie Thompson (GitHub: `jthomperoo`) as the original writer and maintainer of
PHPA, along with all upstream contributors who designed, implemented, and evolved the project over time. HPA+ builds
on that foundation and continues development for our current use cases, while preserving attribution to the original
work and its Apache-2.0 licensed codebase.

## What This Repository Is

This repo contains:

- a Kubernetes operator and Helm chart for `HPAPlus`
- multiple prediction backends, from simple trend-following to boosted trees
- sample manifests for repeatable experiments
- scripts for trace capture, benchmark sweeps, and graphing
- a workflow oriented around tuning from real workload history instead of picking model settings blindly

## Where It Helps

HPA+ is useful when reactive-only scaling is systematically late for the workload you care about.

Some use cases:

- services with repeated daily or weekly ramps
- short spikes where the normal HPA loop reacts after saturation has already started
- workloads with expensive warm-up time, where being slightly early is cheaper than being slightly late
- teams that want to compare multiple predictive models against the same captured history before standardizing on one

## Where It Does Not Help

HPA+ is not a blanket upgrade over HPA.

It is a bad fit when:

- demand is mostly random and not learnable from recent history
- there is not enough runtime history to warm the model families you want to use
- there is no appetite to benchmark and tune against real traces
- operational simplicity matters more than shaving reaction time

If you skip the tuning step, predictive autoscaling can easily be worse than plain HPA.

## Control Loop

At each sync, the operator:

1. computes the baseline desired replicas using HPA-style metric evaluation
2. records the runtime history needed by the configured model family
3. runs the selected model or models against that history
4. optionally keeps the plain HPA baseline in the decision set via `includeHPA`
5. combines the candidate outputs using `decisionType`
6. applies the normal scale constraints such as behavior policies, stabilization windows, and min/max replicas

So HPA+ does not replace the HPA mental model. It inserts prediction into the decision path and keeps the rest of the
autoscaling envelope intact.

## Supported Kubernetes versions

The minimum Kubernetes version the autoscaler can run on is `v1.23` because it relies on the `autoscaling/v2` API which
was only available in `v1.23` and above.

The autoscaler is only tested against the latest Kubernetes version - if there are bugs that affect older Kubernetes
versions we will try to fix them, but there is no guarantee of support.

## Model Families In This Repo

- All current predictors in this repo operate over aggregate CPU-history inputs
- `Linear` and `HoltWinters` are the lighter statistical options
- `ARIMA`, `XGBoost`, and `LightGBM` add progressively more modeling capacity and tuning surface
- the repository includes both sample manifests and benchmark tooling for the tree-based models

See [models.md](./docs/user-guide/models.md) for the current repo-specific model guide.

## Resource Shape

The custom resource keeps the HPA shape on purpose. `scaleTargetRef`, `metrics`, `behavior`, `minReplicas`, and
`maxReplicas` stay familiar; prediction is introduced through `spec.models`.

HPA+ has its own custom resource:

```yaml
apiVersion: hpa.plus/v1alpha1
kind: HPAPlus
metadata:
  name: simple-linear
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: hpa-plus-apache
  minReplicas: 1
  maxReplicas: 10
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 0
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          averageUtilization: 50
          type: Utilization
  models:
    - type: Linear
      name: simple-linear
      linear:
        lookAhead: 10000
        historySize: 6
```

This object still behaves like a CPU-targeted autoscaler. The difference is that the controller now records history for
the `Linear` model and lets that model contribute to the final replica decision.

## Installation

Because the project now lives in a private repository the recommended workflow is to build and deploy the operator from
source:

1. Build (and optionally push) the controller image.

   ```bash
   export REGISTRY=docker.io/<your-user>   # change as needed
   export VERSION=$(git rev-parse --short HEAD)

   docker build -t ${REGISTRY}/hpa-plus-operator:${VERSION} .
   docker push ${REGISTRY}/hpa-plus-operator:${VERSION}    # optional if you use kind/minikube
   ```

2. Install the Helm chart directly from this repository, pointing it at the image you just built.

   ```bash
   helm upgrade --install hpa-plus-operator ./helm \
     --namespace hpa-plus-system \
     --create-namespace \
     --set image.repository=${REGISTRY}/hpa-plus-operator \
     --set image.tag=${VERSION} \
     --set mode=cluster
   ```

3. Create an `HPAPlus` resource using the model configuration from the getting started guide or model reference, then
   apply it with `kubectl`.

## Common workflows

For local development, build the image, install the Helm chart, deploy a workload, and apply an `HPAPlus` resource that
targets that workload. The operator stores model history in ConfigMaps and reconciles the target replica count from the
configured HPA and predictive model inputs.

## Quick start

Check out the [getting started
guide](https://github.com/cslab-ntua/HPA-Plus/tree/master/docs/user-guide/getting-started.md) for a complete local
walkthrough.

## More information

See the [wiki for more information, such as guides and
references](https://github.com/cslab-ntua/HPA-Plus/tree/master/docs).

See the [model reference](https://github.com/cslab-ntua/HPA-Plus/tree/master/docs/user-guide/models.md) for supported
model configuration blocks.

## Developing this project

Developing this project requires these dependencies:

* [Go](https://golang.org/doc/install) >= `1.20`
* [Python](https://www.python.org/downloads/) == `3.8.x`
* [Helm](https://helm.sh/) == `3.9.x`

Any Python dependencies must be installed by running:

```bash
pip install -r requirements-dev.txt
```

This extensively uses the the k8shorizmetrics library
to gather metrics and to evaluate them as the Kubernetes Horizontal Pod Autoscaler does.

It is recommended to test locally using a local Kubernetes managment system, such as
[k3d](https://github.com/rancher/k3d) (allows running a small Kubernetes cluster locally using Docker).

You can test changes by deploying a simple workload, applying an `HPAPlus` object from the getting started guide, and
watching the target deployment's replica count and HPA+ status.

### Commands

* `make py_dependencies` - installs Python development dependencies.
* `make docker` - builds the HPA+ image.
* `make push` - pushes the HPA+ image.
* `make generate` - regenerates Go deepcopy code, RBAC, webhook manifests, and CRDs.
* `make test` - runs the unit tests.
* `make gotest` - runs Go tests with coverage.
* `make pytest` - runs Python algorithm tests with coverage.
* `make view_coverage` - opens up any generated coverage reports in the browser.
