# Attribution

HPA+ is derived from
[jthomperoo/predictive-horizontal-pod-autoscaler](https://github.com/jthomperoo/predictive-horizontal-pod-autoscaler),
originally created by Jamie Thompson and contributors.

This repository has since diverged substantially from the upstream project. The
work includes the HPA+ rebrand, Kubernetes API and CRD renaming, a new API group,
expanded model support, revised autoscaler state and history data structures,
changes to the forecasting inputs and replica decision flow, additional
configuration surfaces, dependency and platform updates, bug fixes on top of the
inherited controller behavior, and refreshed user documentation.

The original project and this derivative work are distributed under the
Apache License, Version 2.0. See [LICENSE](LICENSE).

## Optional model images

The `toto` and `tirex` image targets download third-party packages and pretrained weights at build time; none of
them are stored in this repository.

- Toto 2.0 (Datadog): weights under the Apache License 2.0.
- TiRex-2 (NXAI GmbH): weights under the Apache License 2.0; its LICENSE and NOTICE files are copied into the image
  next to the weights.
- mlstm-kernels (NXAI GmbH), installed by the `tirex` target, is under the NXAI Community License Agreement, not
  Apache 2.0. Redistributing an image built with it requires its attribution notice and, above 100 million euros of
  yearly revenue, a commercial license from NXAI. The license text is in the package's metadata in the image.
