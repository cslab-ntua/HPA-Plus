# Installation

You can install HPA+ on your cluster after you have installed the HPA+ operator onto your cluster.

1. Build or retag the controller image with the registry you plan to use:

   ```bash
   export REGISTRY=docker.io/<your-user>
   export VERSION=$(git rev-parse --short HEAD)

   docker build -t ${REGISTRY}/hpa-plus-operator:${VERSION} .
   docker push ${REGISTRY}/hpa-plus-operator:${VERSION}
   ```

2. Install the Helm chart from this repository while overriding the image fields:

   ```bash
   helm upgrade --install hpa-plus-operator ./helm \
     --namespace hpa-plus-system \
     --create-namespace \
     --set image.repository=${REGISTRY}/hpa-plus-operator \
     --set image.tag=${VERSION} \
     --set mode=cluster
   ```

After you have done that you can install HPA+ objects onto your cluster, check out the [examples you can
deploy](https://github.com/cslab-ntua/HPA-Plus/tree/master/examples) or follow the [getting
started guide](./getting-started.md).
