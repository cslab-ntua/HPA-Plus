#!/usr/bin/env bash
set -euo pipefail

# Namespace containing the sample workloads/ConfigMaps.
: "${WORKLOAD_NAMESPACE:=default}"
# Helm release + namespace for the PHPA operator.
: "${HELM_RELEASE:=predictive-horizontal-pod-autoscaler-operator}"
: "${HELM_NAMESPACE:=phpa-system}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

echo "Deleting PredictiveHorizontalPodAutoscaler in ${WORKLOAD_NAMESPACE}..."
kubectl delete predictivehorizontalpodautoscaler test-arima-scaler \
  -n "${WORKLOAD_NAMESPACE}" \
  --ignore-not-found

echo "Removing sample workloads and services in ${WORKLOAD_NAMESPACE}..."
kubectl delete deployment/test-app \
  deployment/test-app-loadgen \
  -n "${WORKLOAD_NAMESPACE}" \
  --ignore-not-found
kubectl delete service/test-app \
  -n "${WORKLOAD_NAMESPACE}" \
  --ignore-not-found

echo "Deleting ConfigMaps in ${WORKLOAD_NAMESPACE}..."
kubectl delete configmap/test-app-server \
  configmap/test-app-load-script \
  configmap/predictive-horizontal-pod-autoscaler-test-arima-scaler-data \
  -n "${WORKLOAD_NAMESPACE}" \
  --ignore-not-found

echo "Uninstalling PHPA Helm release (${HELM_RELEASE}) from ${HELM_NAMESPACE}..."
helm uninstall "${HELM_RELEASE}" -n "${HELM_NAMESPACE}" || true

echo "Environment cleanup completed."
