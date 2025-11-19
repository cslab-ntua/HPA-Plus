#!/usr/bin/env bash
set -euo pipefail

# Namespace containing the sample workloads/ConfigMaps.
: "${WORKLOAD_NAMESPACE:=default}"
# Helm release + namespace for the PHPA operator.
: "${HELM_RELEASE:=predictive-horizontal-pod-autoscaler-operator}"
: "${HELM_NAMESPACE:=phpa-system}"
# Helm chart mode (cluster | development). Default to cluster to match production-like installs.
: "${HELM_MODE:=cluster}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

echo "Installing/Upgrading PHPA Helm release (${HELM_RELEASE}) in ${HELM_NAMESPACE}..."
helm upgrade --install "${HELM_RELEASE}" "${REPO_ROOT}/helm" \
  --namespace "${HELM_NAMESPACE}" \
  --create-namespace \
  --set mode="${HELM_MODE}"

echo "Deploying CPU-intensive sample app (test-app) to ${WORKLOAD_NAMESPACE}..."
kubectl apply -n "${WORKLOAD_NAMESPACE}" -f "${REPO_ROOT}/test-app-server-configmap.yaml"
kubectl apply -n "${WORKLOAD_NAMESPACE}" -f "${REPO_ROOT}/test-deployment.yaml"

echo "Deploying Vegeta load generator..."
kubectl apply -n "${WORKLOAD_NAMESPACE}" -f "${REPO_ROOT}/test_load.yaml"

echo "Applying PredictiveHorizontalPodAutoscaler spec..."
kubectl apply -n "${WORKLOAD_NAMESPACE}" -f "${REPO_ROOT}/test-arima-phpa.yaml"

echo "Bootstrap completed. Monitor pods with:"
echo "  kubectl get pods -n ${WORKLOAD_NAMESPACE}"
echo "  kubectl get pods -n ${HELM_NAMESPACE}"
