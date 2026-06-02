# Installation

You can install HPA+ on your cluster after you have installed the HPA+ operator onto your cluster.

1. Build and push the controller image with the registry you plan to use:

   ```bash
   export REGISTRY=<your-registry-or-dockerhub-user>
   export VERSION=$(git rev-parse --short HEAD)

   make docker REGISTRY=${REGISTRY} VERSION=${VERSION}
   make push REGISTRY=${REGISTRY} VERSION=${VERSION}
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

After you have done that you can install `HPAPlus` objects onto your cluster. Follow the [getting started
guide](./getting-started.md) for a local smoke test.
