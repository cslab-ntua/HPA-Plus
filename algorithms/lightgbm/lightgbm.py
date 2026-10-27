#!/usr/bin/env python3
"""
Train-and-predict loop for LightGBM using recent aggregate CPU history
(and optional metric history) to forecast the next CPU usage value.
"""

import json
import math
import os
import sys
from dataclasses import dataclass
from importlib import import_module
from typing import List, Optional, Tuple

# Resolve shared helpers for both direct script execution and runpy dispatch.
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
try:
    from forecast_trajectory import grid, emit
    from forecast_diagnostics import stage
    from feature_diagnostics import dump_features
    from forecast_budget import tree_budget
    from forecast_features import build_feature_row, build_training_matrix, forecast, feature_header
    from forecast_time import parse_timestamp, median_interval_ms as shared_interval_ms, forecast_steps as shared_forecast_steps
finally:
    sys.path.pop(0)

import numpy as np
from dataclasses_json import LetterCase, dataclass_json


def _import_lightgbm():
    """Ensure we import the external lightgbm package, not this script."""
    script_dir = os.path.abspath(os.path.dirname(__file__))
    original_path = sys.path[:]
    sys.path[:] = [path for path in sys.path if os.path.abspath(path) != script_dir]
    try:
        return import_module("lightgbm")
    finally:
        sys.path[:] = original_path


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
    return_trajectory: bool = False
    diagnostic_scope: Optional[str] = None
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


def parse_time(timestamp):
    return None if timestamp is None else parse_timestamp(timestamp)


def median_interval_ms(replica_history):
    return shared_interval_ms([parse_time(item.time) for item in replica_history if item.time is not None])


def determine_steps(look_ahead_ms, replica_history):
    return shared_forecast_steps(look_ahead_ms, [parse_time(item.time) for item in replica_history if item.time is not None])


def validate_input(alg_input: AlgorithmInput) -> None:
    """Validate required parameters and available data."""
    if alg_input.objective not in (None, "regression", "regression_l2", "l2", "mean_squared_error", "mse",
                                   "regression_l1", "l1", "mean_absolute_error", "mae", "huber", "fair",
                                   "poisson", "quantile", "mape", "mean_absolute_percentage_error", "gamma", "tweedie"):
        print("Unsupported CPU regression objective", file=sys.stderr)
        sys.exit(1)
    if alg_input.look_ahead <= 0:
        print("lookAhead must be > 0", file=sys.stderr)
        sys.exit(1)
    if alg_input.lags <= 0:
        print("lags must be > 0", file=sys.stderr)
        sys.exit(1)
    if not alg_input.replica_history:
        print("No replica history provided", file=sys.stderr)
        sys.exit(1)
    for name in ("learning_rate", "subsample", "colsample_bytree", "reg_lambda", "reg_alpha"):
        value = getattr(alg_input, name)
        if value is not None and not math.isfinite(value):
            print(f"{name} must be finite", file=sys.stderr)
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


def emit_prediction(value: float) -> None:
    """Print integer prediction without trailing newline so Go parser stays happy."""
    sys.stdout.write(str(int(round(value))))
    sys.stdout.flush()


def maybe_dump_features(X_train: np.ndarray, rolling_window: int, metrics_used: bool,
                        scope: Optional[str] = None) -> None:
    if os.environ.get("HPAPLUS_FEATURE_DUMP_DIR") and scope:
        dump_features("lightgbm", scope, feature_header(rolling_window, metrics_used), X_train)


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
    tree_budget("LightGBM", len(alg_input.replica_history), alg_input.lags,
                alg_input.window_size, alg_input.metric_history is not None, alg_input.n_estimators, alg_input.max_depth,
                alg_input.num_leaves)

    sorted_history, sorted_metrics = sort_history(alg_input.replica_history, alg_input.metric_history)
    steps = determine_steps(alg_input.look_ahead, sorted_history)
    if alg_input.return_trajectory:
        grid([r.time for r in sorted_history], alg_input.look_ahead)
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

    maybe_dump_features(X_train, rolling_window, sorted_metrics is not None, alg_input.diagnostic_scope)

    model = lgb.LGBMRegressor(**build_model_params(alg_input))
    with stage("training"):
        model.fit(X_train, y_train)

    with stage("inference"):
        prediction = forecast(model, series, sorted_metrics, alg_input.lags, steps, rolling_window, return_values=alg_input.return_trajectory)
    if alg_input.return_trajectory:
        emit(max(0, int(round(prediction[-1]))), prediction, [r.time for r in sorted_history], alg_input.look_ahead)
        return
    clamped = max(0, int(round(prediction)))
    emit_prediction(clamped)


if __name__ == "__main__":
    main()
