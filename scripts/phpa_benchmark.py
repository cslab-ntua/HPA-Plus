#!/usr/bin/env python3
"""
Benchmark recorder for Predictive Horizontal Pod Autoscaler (PHPA) runs.

This script samples, at a fixed interval:
* The desired replica count chosen by PHPA (the "HPA decision").
* The ARIMA model prediction, reproduced by invoking the bundled algorithm with the same history/config.
* Aggregate CPU usage across all pods managed by the target Deployment (via `kubectl top pods`).

All samples are written to a CSV file with UTC timestamps.
"""

import argparse
import csv
import json
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Dict, List, Optional

from kubernetes import client, config

GROUP = "jamiethompson.me"
VERSION = "v1alpha1"
PLURAL = "predictivehorizontalpodautoscalers"


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="PHPA benchmark recorder")
    parser.add_argument("--namespace", required=True, help="Namespace of the PHPA and target workload")
    parser.add_argument("--phpa-name", required=True, help="Name of the PredictiveHorizontalPodAutoscaler")
    parser.add_argument("--deployment", required=True, help="Name of the target Deployment")
    parser.add_argument("--model-name", required=True, help="Name of the ARIMA model inside the PHPA spec")
    parser.add_argument("--duration-seconds", type=int, default=3600, help="Benchmark duration (default: 3600)")
    parser.add_argument("--sample-interval", type=int, default=15, help="Sampling interval in seconds (default: 15)")
    parser.add_argument("--output", required=True, help="Path to the CSV file to write")
    parser.add_argument(
        "--kubeconfig",
        help="Optional path to kubeconfig; defaults to standard lookups used by kubernetes-client",
    )
    return parser.parse_args()


def load_clients(kubeconfig: Optional[str]) -> Dict[str, object]:
    if kubeconfig:
        config.load_kube_config(config_file=kubeconfig)
    else:
        config.load_kube_config()

    return {
        "custom": client.CustomObjectsApi(),
        "core": client.CoreV1Api(),
        "apps": client.AppsV1Api(),
    }


def read_phpa(custom_api: client.CustomObjectsApi, namespace: str, name: str) -> Dict:
    return custom_api.get_namespaced_custom_object(GROUP, VERSION, namespace, PLURAL, name)


def extract_model_spec(phpa: Dict, model_name: str) -> Dict:
    for model in phpa["spec"]["models"]:
        if model["name"] == model_name:
            if model["type"] != "ARIMA":
                raise ValueError(f"Model '{model_name}' is type {model['type']}, expected ARIMA.")
            return model
    raise ValueError(f"Model '{model_name}' not found in PHPA spec.")


def get_deployment_selector(apps_api: client.AppsV1Api, namespace: str, deployment: str) -> str:
    dep = apps_api.read_namespaced_deployment(deployment, namespace)
    match_labels = dep.spec.selector.match_labels
    return ",".join(f"{k}={v}" for k, v in match_labels.items())


def read_model_history(core_api: client.CoreV1Api, namespace: str, phpa_name: str, model_name: str) -> List[Dict]:
    cm_name = f"predictive-horizontal-pod-autoscaler-{phpa_name}-data"
    cm = core_api.read_namespaced_config_map(cm_name, namespace)
    payload = json.loads(cm.data["data"])
    history = payload["modelHistories"][model_name]["replicaHistory"]
    return history


def run_arima_prediction(model_spec: Dict, history: List[Dict]) -> Optional[int]:
    arima_config = model_spec.get("arima") or {}
    if not arima_config:
        return None

    # Defaults mirror internal/prediction/arima/arima.go
    auto_arima = arima_config.get("autoArima", False)
    information_criterion = arima_config.get("informationCriterion", "aic")
    enforce_stationarity = arima_config.get("enforceStationarity", True)
    enforce_invertibility = arima_config.get("enforceInvertibility", True)
    concentrate_scale = arima_config.get("concentrateScale", False)

    payload = {
        "order": arima_config["order"],
        "lookAhead": arima_config["lookAhead"],
        "replicaHistory": history,
        "trend": arima_config.get("trend"),
        "autoArima": auto_arima,
        "informationCriterion": information_criterion,
        "maxOrder": arima_config.get("maxOrder"),
        "enforceStationarity": enforce_stationarity,
        "enforceInvertibility": enforce_invertibility,
        "concentrateScale": concentrate_scale,
    }

    repo_root = Path(__file__).resolve().parents[1]
    script_path = repo_root / "algorithms" / "arima" / "arima.py"
    try:
        proc = subprocess.run(
            ["python", str(script_path)],
            input=json.dumps(payload),
            text=True,
            capture_output=True,
            check=False,
        )
    except FileNotFoundError:
        print("Python executable not found when invoking ARIMA helper.", file=sys.stderr)
        return None

    if proc.returncode != 0:
        print(f"ARIMA script failed: {proc.stderr.strip()}", file=sys.stderr)
        return None

    try:
        return int(proc.stdout.strip())
    except ValueError:
        print(f"Unable to parse ARIMA output: {proc.stdout}", file=sys.stderr)
        return None


def collect_cpu_total(namespace: str, selector: str) -> Optional[int]:
    cmd = [
        "kubectl",
        "top",
        "pods",
        "-n",
        namespace,
        "-l",
        selector,
        "--no-headers",
    ]
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        print(f"`kubectl top pods` failed: {proc.stderr.strip()}", file=sys.stderr)
        return None

    total_mcores = 0
    for line in proc.stdout.strip().splitlines():
        parts = line.split()
        if len(parts) < 2:
            continue
        cpu_val = parts[1]
        if cpu_val.endswith("m"):
            cpu_val = cpu_val[:-1]
        try:
            total_mcores += int(cpu_val)
        except ValueError:
            continue
    return total_mcores


def main() -> None:
    args = parse_args()
    clients = load_clients(args.kubeconfig)

    phpa_obj = read_phpa(clients["custom"], args.namespace, args.phpa_name)
    model_spec = extract_model_spec(phpa_obj, args.model_name)
    selector = get_deployment_selector(clients["apps"], args.namespace, args.deployment)

    csv_path = Path(args.output).expanduser().resolve()
    csv_path.parent.mkdir(parents=True, exist_ok=True)

    end_time = time.time() + args.duration_seconds

    with csv_path.open("w", newline="") as csv_file:
        writer = csv.writer(csv_file)
        writer.writerow(
            [
                "timestamp",
                "plain_hpa_replicas",
                "arima_prediction",
                "applied_replicas",
                "total_cpu_mcores",
            ]
        )

        while time.time() < end_time:
            tick_start = time.time()
            try:
                phpa_obj = read_phpa(clients["custom"], args.namespace, args.phpa_name)
                desired = phpa_obj.get("status", {}).get("desiredReplicas")
            except Exception as exc:
                print(f"Failed to fetch PHPA status: {exc}", file=sys.stderr)
                desired = None

            try:
                history = read_model_history(
                    clients["core"], args.namespace, args.phpa_name, args.model_name
                )
            except Exception as exc:
                print(f"Failed to read model history: {exc}", file=sys.stderr)
                history = []

            plain_hpa = history[-1]["replicas"] if history else None
            arima_prediction = run_arima_prediction(model_spec, history) if history else None
            cpu_total = collect_cpu_total(args.namespace, selector)

            writer.writerow(
                [
                    datetime.now(timezone.utc).isoformat(),
                    plain_hpa if plain_hpa is not None else "",
                    arima_prediction if arima_prediction is not None else "",
                    desired if desired is not None else "",
                    cpu_total if cpu_total is not None else "",
                ]
            )
            csv_file.flush()

            elapsed = time.time() - tick_start
            sleep_for = max(0, args.sample_interval - elapsed)
            time.sleep(sleep_for)

    print(f"Benchmark complete. Data written to {csv_path}")


if __name__ == "__main__":
    main()
