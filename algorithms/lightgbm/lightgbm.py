#!/usr/bin/env python3
"""
Train-and-predict loop for LightGBM using recent aggregate CPU history
(and optional metric history) to forecast the next CPU usage value.
"""

import csv
import json
import math
import os
import statistics
import sys
from dataclasses import dataclass
from datetime import datetime
from importlib import import_module
from typing import List, Optional, Tuple

import numpy as np
from dataclasses_json import LetterCase, dataclass_json


def _import_lightgbm():
    """Ensure we import the external lightgbm package, not this script."""
    script_dir = os.path.abspath(os.path.dirname(__file__))
    removed = False
    if sys.path and os.path.abspath(sys.path[0]) == script_dir:
        sys.path.pop(0)
        removed = True
    try:
        return import_module("lightgbm")
    finally:
        if removed:
            sys.path.insert(0, script_dir)


lgb = _import_lightgbm()


@dataclass_json(letter_case=LetterCase.CAMEL)
@dataclass
class TimestampedReplica:
    time: Optional[str]
    replicas: int
    metric: Optional[float] = None
    total_cpu_usage_millicores: Optional[int] = None
    request_per_pod_millicores: Optional[int] = None
    target_cpu_utilization_percentage: Optional[int] = None


@dataclass_json(letter_case=LetterCase.CAMEL)
@dataclass
class AlgorithmInput:
    look_ahead: int
    lags: int
    replica_history: List[TimestampedReplica]
    metric_history: Optional[List[float]] = None
    window_size: Optional[int] = None
    max_depth: Optional[int] = None
    n_estimators: Optional[int] = None
    learning_rate: Optional[float] = None
    subsample: Optional[float] = None
    colsample_bytree: Optional[float] = None
    objective: Optional[str] = None
    num_leaves: Optional[int] = None
    min_child_samples: Optional[int] = None
    reg_lambda: Optional[float] = None
    reg_alpha: Optional[float] = None


def read_input() -> AlgorithmInput:
    """Read JSON from stdin and deserialize."""
    if sys.stdin.isatty():
        print("No standard input provided to LightGBM algorithm, exiting", file=sys.stderr)
        sys.exit(1)

    try:
        raw = sys.stdin.read()
        return AlgorithmInput.schema().loads(raw)
    except json.JSONDecodeError as exc:
        print(f"Failed to parse JSON input: {exc}", file=sys.stderr)
        sys.exit(1)
    except Exception as exc:  # pylint: disable=broad-except
        print(f"Failed to load input: {exc}", file=sys.stderr)
        sys.exit(1)


def parse_time(timestamp: Optional[str]) -> Optional[float]:
    """Parse ISO 8601 time string to a UNIX timestamp in seconds."""
    if timestamp is None:
        return None
    try:
        return datetime.timestamp(datetime.strptime(timestamp, "%Y-%m-%dT%H:%M:%SZ"))
    except ValueError as exc:
        print(f"Invalid datetime format: {exc}", file=sys.stderr)
        return None


def median_interval_ms(replica_history: List[TimestampedReplica]) -> int:
    """Calculate median interval between timestamps in milliseconds; default to 1000 if unavailable."""
    timestamps = [parse_time(item.time) for item in replica_history if item.time is not None]
    if len(timestamps) < 2:
        return 1000

    deltas = [t2 - t1 for t1, t2 in zip(timestamps, timestamps[1:]) if (t2 - t1) > 0]
    if not deltas:
        return 1000

    med = statistics.median(deltas)
    return max(int(round(med * 1000)), 1)


def determine_steps(look_ahead_ms: int, replica_history: List[TimestampedReplica]) -> int:
    """Translate look-ahead milliseconds into forecast steps."""
    interval = median_interval_ms(replica_history)
    return max(1, int(round(look_ahead_ms / interval)))


def validate_input(alg_input: AlgorithmInput) -> None:
    """Validate required parameters and available data."""
    if alg_input.look_ahead <= 0:
        print("lookAhead must be > 0", file=sys.stderr)
        sys.exit(1)
    if alg_input.lags <= 0:
        print("lags must be > 0", file=sys.stderr)
        sys.exit(1)
    if not alg_input.replica_history:
        print("No replica history provided", file=sys.stderr)
        sys.exit(1)
    if alg_input.window_size is not None:
        if alg_input.window_size <= 0:
            print("windowSize must be > 0 when provided", file=sys.stderr)
            sys.exit(1)
        if alg_input.window_size > alg_input.lags:
            print("windowSize must be <= lags", file=sys.stderr)
            sys.exit(1)
    if alg_input.max_depth is not None and alg_input.max_depth != -1 and alg_input.max_depth <= 0:
        print("maxDepth must be -1 or > 0 when provided", file=sys.stderr)
        sys.exit(1)
    if alg_input.n_estimators is not None and alg_input.n_estimators <= 0:
        print("nEstimators must be > 0 when provided", file=sys.stderr)
        sys.exit(1)
    if alg_input.learning_rate is not None and alg_input.learning_rate <= 0:
        print("learningRate must be > 0 when provided", file=sys.stderr)
        sys.exit(1)
    if alg_input.subsample is not None and not 0.0 < alg_input.subsample <= 1.0:
        print("subsample must be in (0, 1] when provided", file=sys.stderr)
        sys.exit(1)
    if alg_input.colsample_bytree is not None and not 0.0 < alg_input.colsample_bytree <= 1.0:
        print("colsampleBytree must be in (0, 1] when provided", file=sys.stderr)
        sys.exit(1)
    if alg_input.num_leaves is not None and alg_input.num_leaves < 2:
        print("numLeaves must be >= 2 when provided", file=sys.stderr)
        sys.exit(1)
    if alg_input.min_child_samples is not None and alg_input.min_child_samples < 1:
        print("minChildSamples must be >= 1 when provided", file=sys.stderr)
        sys.exit(1)
    if alg_input.reg_lambda is not None and alg_input.reg_lambda < 0:
        print("regLambda must be >= 0 when provided", file=sys.stderr)
        sys.exit(1)
    if alg_input.reg_alpha is not None and alg_input.reg_alpha < 0:
        print("regAlpha must be >= 0 when provided", file=sys.stderr)
        sys.exit(1)
    if len(alg_input.replica_history) <= alg_input.lags:
        return
    if alg_input.metric_history is not None and len(alg_input.metric_history) != len(alg_input.replica_history):
        print("metricHistory must match length of replicaHistory when provided", file=sys.stderr)
        sys.exit(1)


def sort_history(
    replica_history: List[TimestampedReplica],
    metric_history: Optional[List[float]],
) -> Tuple[List[TimestampedReplica], Optional[List[float]]]:
    """Sort history chronologically and reorder metric_history to match."""
    paired = []
    for idx, replica in enumerate(replica_history):
        ts = parse_time(replica.time)
        paired.append((ts if ts is not None else float(idx), idx, replica))
    paired.sort(key=lambda item: item[0])

    sorted_replicas = [entry[2] for entry in paired]
    if metric_history is None:
        return sorted_replicas, None

    sorted_metrics = [metric_history[entry[1]] for entry in paired]
    return sorted_replicas, sorted_metrics


def extract_cpu_usage_series(replica_history: List[TimestampedReplica]) -> List[float]:
    """Extract aggregate CPU usage history from sorted replica history."""
    series = []
    for replica in replica_history:
        if replica.total_cpu_usage_millicores is None:
            print("Missing totalCpuUsageMillicores in replica history, exiting", file=sys.stderr)
            sys.exit(1)
        series.append(float(replica.total_cpu_usage_millicores))
    return series


def build_feature_row(rep_window: List[float], metric_window: Optional[List[float]]) -> List[float]:
    """Compose a single feature row from CPU and metric windows."""
    row: List[float] = []
    row.extend(rep_window)
    if len(rep_window) > 1:
        diffs = np.diff(rep_window)
        row.extend(diffs.tolist())
        row.append(float(np.mean(rep_window)))
        row.append(float(np.std(rep_window)))
    else:
        row.extend([0.0])
        row.append(float(rep_window[0]))
        row.append(0.0)

    if metric_window:
        row.extend(metric_window)
        if len(metric_window) > 1:
            row.append(float(np.mean(metric_window)))
            row.append(float(np.std(metric_window)))
        else:
            row.append(float(metric_window[0]))
            row.append(0.0)

    return row


def build_training_matrix(
    series: List[float],
    metrics: Optional[List[float]],
    lags: int,
    rolling_window: int,
) -> Tuple[np.ndarray, np.ndarray]:
    """Create supervised learning matrix using lagged features."""
    X, y = [], []
    for idx in range(lags, len(series)):
        cpu_window = series[idx - lags:idx]
        metric_window = metrics[idx - lags:idx] if metrics else None

        cpu_slice = cpu_window[-rolling_window:]
        metric_slice = metric_window[-rolling_window:] if metric_window else None

        row = build_feature_row(cpu_slice, metric_slice)
        X.append(row)
        y.append(series[idx])
    return np.array(X), np.array(y)


def forecast(
    model: lgb.LGBMRegressor,
    history: List[float],
    metrics: Optional[List[float]],
    lags: int,
    steps: int,
    rolling_window: int,
) -> float:
    """Iteratively forecast forward and return the final predicted CPU usage."""
    cpu_series = list(history)
    metric_series = list(metrics) if metrics else None

    for _ in range(steps):
        if len(cpu_series) < lags:
            return cpu_series[-1]

        cpu_window = cpu_series[-lags:]
        metric_window = metric_series[-lags:] if metric_series else None

        cpu_slice = cpu_window[-rolling_window:]
        metric_slice = metric_window[-rolling_window:] if metric_window else None
        features = np.array([build_feature_row(cpu_slice, metric_slice)])

        next_val = float(model.predict(features)[0])
        cpu_series.append(next_val)

        if metric_series is not None:
            metric_series.append(metric_series[-1])

    return cpu_series[-1]


def emit_prediction(value: float) -> None:
    """Print integer prediction without trailing newline so Go parser stays happy."""
    sys.stdout.write(str(int(round(value))))
    sys.stdout.flush()


def maybe_dump_features(X_train: np.ndarray, rolling_window: int, metrics_used: bool) -> None:
    """Append feature matrix to CSV for debugging at a fixed path."""
    path = os.environ.get("LGBM_FEATURE_DUMP", "/tmp/lgbm_features.csv")
    os.makedirs(os.path.dirname(path), exist_ok=True)

    cpu_header = [f"cpu_lag_{i}" for i in range(rolling_window)]
    cpu_diff_header = [f"cpu_diff_{i}" for i in range(rolling_window - 1)] if rolling_window > 1 else []
    header = cpu_header + cpu_diff_header + ["cpu_mean", "cpu_std"]

    if metrics_used:
        metric_header = [f"metric_lag_{i}" for i in range(rolling_window)]
        metric_stats_header = ["metric_mean", "metric_std"]
        header += metric_header + metric_stats_header

    file_exists = os.path.isfile(path)
    with open(path, "a", newline="", encoding="utf-8") as handle:
        writer = csv.writer(handle)
        if not file_exists:
            writer.writerow(header)
        writer.writerows(X_train.tolist())


def build_model_params(alg_input: AlgorithmInput) -> dict:
    """Build LightGBM hyperparameters with small-dataset-friendly defaults."""
    subsample = alg_input.subsample if alg_input.subsample is not None else 0.8
    params = {
        "n_estimators": alg_input.n_estimators if alg_input.n_estimators is not None else 200,
        "max_depth": alg_input.max_depth if alg_input.max_depth is not None else 4,
        "learning_rate": alg_input.learning_rate if alg_input.learning_rate is not None else 0.1,
        "subsample": subsample,
        "subsample_freq": 1 if subsample < 1.0 else 0,
        "colsample_bytree": alg_input.colsample_bytree if alg_input.colsample_bytree is not None else 1.0,
        "objective": alg_input.objective if alg_input.objective is not None else "regression",
        "num_leaves": alg_input.num_leaves if alg_input.num_leaves is not None else 31,
        "min_child_samples": alg_input.min_child_samples if alg_input.min_child_samples is not None else 20,
        "reg_lambda": alg_input.reg_lambda if alg_input.reg_lambda is not None else 1.0,
        "reg_alpha": alg_input.reg_alpha if alg_input.reg_alpha is not None else 0.0,
        "n_jobs": 1,
        "verbosity": -1,
        "random_state": 42,
    }
    return params


def main() -> None:
    alg_input = read_input()
    validate_input(alg_input)

    sorted_history, sorted_metrics = sort_history(alg_input.replica_history, alg_input.metric_history)
    series = extract_cpu_usage_series(sorted_history)

    if len(series) <= alg_input.lags:
        emit_prediction(series[-1])
        return

    rolling_window = alg_input.window_size if alg_input.window_size is not None else alg_input.lags
    rolling_window = max(1, min(rolling_window, alg_input.lags))

    X_train, y_train = build_training_matrix(series, sorted_metrics, alg_input.lags, rolling_window)
    if X_train.size == 0 or y_train.size == 0:
        emit_prediction(series[-1])
        return

    maybe_dump_features(X_train, rolling_window, sorted_metrics is not None)

    model = lgb.LGBMRegressor(**build_model_params(alg_input))
    model.fit(X_train, y_train)

    steps = determine_steps(alg_input.look_ahead, sorted_history)
    prediction = forecast(model, series, sorted_metrics, alg_input.lags, steps, rolling_window)
    clamped = max(0, int(round(prediction)))
    emit_prediction(clamped)


if __name__ == "__main__":
    main()
