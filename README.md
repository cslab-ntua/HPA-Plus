[![Build](https://github.com/cslab-ntua/HPA-Plus/workflows/main/badge.svg)](https://github.com/cslab-ntua/HPA-Plus/actions)
[![go.dev](https://img.shields.io/badge/go.dev-reference-007d9c?logo=go&logoColor=white&style=flat)](https://pkg.go.dev/github.com/cslab-ntua/HPA-Plus)
[![Go Report Card](https://goreportcard.com/badge/github.com/cslab-ntua/HPA-Plus)](https://goreportcard.com/report/github.com/cslab-ntua/HPA-Plus)
[![Docs](https://img.shields.io/badge/docs-GitHub-blue)](https://github.com/cslab-ntua/HPA-Plus/tree/master/docs)
[![License](https://img.shields.io/:license-apache-blue.svg)](https://www.apache.org/licenses/LICENSE-2.0.html)

# HPA+

HPA+ is a Horizontal Pod Autoscaler (HPA) with predictive capabilities, allowing you to autoscale using statistical
models so you can react ahead of time.

## Why would I use it?

HPA+ can deliver better scaling results by making proactive decisions to scale up ahead of demand, meaning that a
resource does not have to wait for performance to degrade before autoscaling kicks in.

## What systems would need it?

Any systems that have regular/predictable demand peaks/troughs.

Some use cases:

* A service that sees demand peak between 3pm and 5pm every week day, this is a regular and predictable load which
could be pre-empted.
* A service which sees a surge in demand at 12pm every day for 10 minutes, this is such a short time interval that
by the time a regular HPA made the decision to scale up there could already be major performance/availablity issues.

HPA+ is not a silver bullet, and requires tuning using real data for there to be any benefits of using it. A poorly
tuned HPA+ setup could easily end up being worse than a normal HPA.

## How does it work?

This project works by doing the same calculations as the Horizontal Pod Autoscaler does to determine how many replicas
a resource should have, then applies statistical models against the calculated replica count and the replica history.

## Supported Kubernetes versions

The minimum Kubernetes version the autoscaler can run on is `v1.23` because it relies on the `autoscaling/v2` API which
was only available in `v1.23` and above.

The autoscaler is only tested against the latest Kubernetes version - if there are bugs that affect older Kubernetes
versions we will try to fix them, but there is no guarantee of support.

## Features

* Functionally identical to Horizontal Pod Autoscaler for calculating replica counts without prediction.
* Choice of statistical models to apply over Horizontal Pod Autoscaler replica counting logic.
  * Holt-Winters Smoothing
  * Linear Regression
* Allows customisation of Kubernetes autoscaling options without master node access. Can therefore work on managed
solutions such as EKS or GCP.
  * CPU Initialization Period.
  * Downscale Stabilization.
  * Sync Period.

## What does HPA+ look like?

HPA+ objects are designed to be as similar in configuration to Horizontal Pod Autoscalers as possible, with extra
configuration options.

HPA+ has its own custom resource:

```yaml
apiVersion: jamiethompson.me/v1alpha1
kind: PredictiveHorizontalPodAutoscaler
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

This HPA+ object acts like a Horizontal Pod Autoscaler and autoscales to try and keep the target resource's CPU utilization at
50%, but with the extra predictive layer of a linear regression model applied to the results.

## Installation

The operator for managing HPA+ can be installed using Helm:

```bash
VERSION=v0.13.2
HELM_CHART=hpa-plus-operator
helm install ${HELM_CHART} https://github.com/cslab-ntua/HPA-Plus/releases/download/${VERSION}/hpa-plus-${VERSION}.tgz
```

## Quick start

Check out the [getting started
guide](https://github.com/cslab-ntua/HPA-Plus/tree/master/docs/user-guide/getting-started.md) and the
[examples](./examples/) for ways to use HPA+.

## More information

See the [wiki for more information, such as guides and
references](https://github.com/cslab-ntua/HPA-Plus/tree/master/docs).

See the [`examples/` directory](./examples) for working code samples.

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

You can deploy an HPA+ example (see the [`examples/` directory](./examples) for choices) to test your changes.

### Commands

* `make run` - runs HPA+ locally against the cluster configured in your kubeconfig file.
* `make docker` - builds the HPA+ image.
* `make lint` - lints the code.
* `make format` - beautifies the code, must be run to pass the CI.
* `make test` - runs the unit tests.
* `make doc` - hosts the documentation locally at <https://localhost:8000>.
* `make coverage` - opens up any generated coverage reports in the browser.
