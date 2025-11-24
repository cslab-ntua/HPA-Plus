#!/usr/bin/env python3
"""
Dump the raw HPA replica history stored in the PHPA ConfigMap to a CSV file.

Each model keeps a history of the plain HPA calculated replicas* that are fed
into the predictive model. This script reads that history directly from the
ConfigMap (`predictive-horizontal-pod-autoscaler-<phpa>-data`) and emits a CSV
with columns: timestamp,replicas.

*These are the replicas calculated by the underlying HPA logic before PHPA
applies any model adjustments.
"""

import argparse
import csv
import json
from pathlib import Path
from typing import Dict, List, Optional

from kubernetes import client, config

DATA_KEY = "data"


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Dump PHPA model replica history (plain HPA decisions) to CSV."
    )
    parser.add_argument("--namespace", required=True, help="Namespace of the PHPA")
    parser.add_argument("--phpa-name", required=True, help="Name of the PHPA resource")
    parser.add_argument(
        "--model-name",
        required=True,
        help="Name of the model inside the PHPA whose history to export",
    )
    parser.add_argument(
        "--output",
        required=True,
        help="Path to write the CSV (will be overwritten if it exists)",
    )
    parser.add_argument(
        "--kubeconfig",
        help="Optional kubeconfig path. Defaults to standard kubernetes-client lookup.",
    )
    return parser.parse_args()


def load_clients(kubeconfig: Optional[str]) -> Dict[str, client.CoreV1Api]:
    if kubeconfig:
        config.load_kube_config(config_file=kubeconfig)
    else:
        config.load_kube_config()
    return {
        "core": client.CoreV1Api(),
    }


def read_history(
    core_api: client.CoreV1Api, namespace: str, phpa_name: str, model_name: str
) -> List[Dict]:
    cm_name = f"predictive-horizontal-pod-autoscaler-{phpa_name}-data"
    cm = core_api.read_namespaced_config_map(cm_name, namespace)
    if DATA_KEY not in cm.data:
        raise KeyError(f"ConfigMap {cm_name} missing '{DATA_KEY}' key")

    payload = json.loads(cm.data[DATA_KEY])
    model_histories = payload.get("modelHistories") or {}
    if model_name not in model_histories:
        raise KeyError(
            f"Model '{model_name}' not found in PHPA '{phpa_name}' history. "
            f"Available: {', '.join(model_histories.keys()) or 'none'}"
        )
    return model_histories[model_name].get("replicaHistory") or []


def write_csv(rows: List[Dict], output_path: Path) -> None:
    output_path.parent.mkdir(parents=True, exist_ok=True)
    with output_path.open("w", newline="") as csv_file:
        writer = csv.writer(csv_file)
        writer.writerow(["timestamp", "replicas"])
        for entry in rows:
            timestamp = ""
            time_field = entry.get("time")
            if isinstance(time_field, str):
                timestamp = time_field
            elif isinstance(time_field, dict):
                timestamp = time_field.get("time", "")
            writer.writerow([timestamp, entry.get("replicas", "")])


def main() -> None:
    args = parse_args()
    clients = load_clients(args.kubeconfig)

    replica_history = read_history(
        clients["core"], args.namespace, args.phpa_name, args.model_name
    )

    output_path = Path(args.output).expanduser().resolve()
    write_csv(replica_history, output_path)

    print(
        f"Wrote {len(replica_history)} history points for model '{args.model_name}' "
        f"to {output_path}"
    )


if __name__ == "__main__":
    main()
