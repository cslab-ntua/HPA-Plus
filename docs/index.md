[![Docs](https://img.shields.io/badge/docs-GitHub-blue)](https://github.com/cslab-ntua/HPA-Plus/tree/master/docs)
[![License](https://img.shields.io/:license-apache-blue.svg)](https://www.apache.org/licenses/LICENSE-2.0.html)

# HPA+

HPA+ is a benchmark-first predictive autoscaling operator for Kubernetes. It keeps the familiar HPA control shape while
recording runtime history and letting configured forecasting models contribute to replica decisions.

## Why would I use it?

HPA+ can deliver better scaling results when reactive-only scaling is systematically late. It can make proactive
decisions ahead of repeated demand patterns, short spikes, or workloads with expensive warm-up time.

## What systems would need it?

Any systems that have regular or learnable demand peaks/troughs.

Some use cases:

* A service that sees demand peak between 3pm and 5pm every week day, this is a regular and predictable load which
could be pre-empted.
* A service which sees a surge in demand at 12pm every day for 10 minutes, this is such a short time interval that
by the time a regular HPA made the decision to scale up there could already be major performance/availablity issues.

HPA+ is not a silver bullet, and requires tuning using real data for there to be any benefits of using it. A poorly
tuned HPA+ setup can easily be worse than a normal HPA.

## How does it work?

This project works by doing the same calculations as the Horizontal Pod Autoscaler does to determine how many replicas
a resource should have, then applies predictive models against recorded runtime history. In the current repository, the
models learn from aggregate CPU usage history and convert their forecasts back into replica targets.

## Supported Kubernetes versions

The minimum Kubernetes version the autoscaler can run on is `v1.23` because it relies on the `autoscaling/v2` API which
was only available in `v1.23` and above.

The autoscaler is only tested against the latest Kubernetes version - if there are bugs that affect older Kubernetes
versions we will try to fix them, but there is no guarantee of support.

## Features

* Functionally identical to Horizontal Pod Autoscaler for calculating replica counts without prediction.
* Choice of predictive models to apply over Horizontal Pod Autoscaler replica counting logic.
  * ARIMA
  * Holt-Winters Smoothing
  * Linear Regression
  * XGBoost
  * LightGBM
* Allows customisation of Kubernetes autoscaling options without master node access. Can therefore work on managed
solutions such as EKS or GCP.
  * CPU Initialization Period.
  * Downscale Stabilization.
  * Sync Period.
* HPA+ resources use `apiVersion: hpa.plus/v1alpha1` and `kind: HPAPlus`.
