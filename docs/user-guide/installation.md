# Installation

You can install HPA+ on your cluster after you have installed the HPA+ operator onto your cluster.

The HPA+ operator can be installed using Helm, run this command to install the operator onto your cluster with
cluster-wide scope:

```bash
VERSION=v0.13.2
HELM_CHART=hpa-plus-operator
helm install ${HELM_CHART} https://github.com/cslab-ntua/HPA-Plus/releases/download/${VERSION}/hpa-plus-${VERSION}.tgz
```

After you have done that you can install HPA+ objects onto your cluster, check out the [examples you can
deploy](https://github.com/cslab-ntua/HPA-Plus/tree/master/examples) or follow the [getting
started guide](./getting-started.md).
