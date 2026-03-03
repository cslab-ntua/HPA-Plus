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
