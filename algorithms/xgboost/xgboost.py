#!/usr/bin/env python3
"""
Train-and-predict loop for XGBoost using recent aggregate CPU history
(and optional metric history) to forecast the next CPU usage value.
"""

import json
import math
import os
import statistics
import sys
from dataclasses import dataclass
from importlib import import_module
from typing import List, Optional, Tuple

# Resolve shared helpers for both direct script execution and runpy dispatch.
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
try:
    from forecast_diagnostics import stage
    from feature_diagnostics import dump_features
    from forecast_budget import tree_budget
    from forecast_features import build_feature_row, build_training_matrix, forecast, feature_header
    from forecast_time import parse_timestamp, median_interval_ms as shared_interval_ms, forecast_steps as shared_forecast_steps
    from forecast_seasonality import estimate_period, phase_features
    from forecast_direct import direct_matrices, direct_forecast
    from forecast_trends import trend_feature_header
    from forecast_censored_cpu import fit_cpu_aft
finally:
    sys.path.pop(0)

import numpy as np
from dataclasses_json import LetterCase, dataclass_json


def _import_xgboost():
    """Ensure we import the external xgboost package, not this script."""
    script_dir = os.path.abspath(os.path.dirname(__file__))
    original_path = sys.path[:]
    sys.path[:] = [path for path in sys.path if os.path.abspath(path) != script_dir]
    try:
        return import_module("xgboost")
    finally:
        sys.path[:] = original_path


xgb = _import_xgboost()


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
    forecast_aggregation: Optional[str] = None
    seasonal_features: Optional[bool] = None
    forecast_strategy: Optional[str] = None
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
    quantile_alpha: Optional[float] = None
    cpu_demand_weight_exponent: Optional[float] = None
    aft_loss_distribution_scale: Optional[float] = None
    cpu_censoring_utilization_threshold: Optional[float] = None
    min_child_weight: Optional[float] = None
    gamma: Optional[float] = None
    reg_lambda: Optional[float] = None
    reg_alpha: Optional[float] = None


def read_input() -> AlgorithmInput:
    """Read JSON from stdin and deserialize."""
    if sys.stdin.isatty():
        print("No standard input provided to XGBoost algorithm, exiting", file=sys.stderr)
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
    power = alg_input.cpu_demand_weight_exponent
    if power is not None and (type(power) not in (int, float) or
            not math.isfinite(power) or not 0 <= power <= 2):
        raise ValueError("cpuDemandWeightExponent must be finite and in [0, 2]")
    if alg_input.forecast_strategy not in (None, "recursive", "direct"):
        print("forecastStrategy must be recursive or direct", file=sys.stderr)
        sys.exit(1)
    if alg_input.seasonal_features is not None and type(alg_input.seasonal_features) is not bool:
        print("seasonalFeatures must be boolean", file=sys.stderr)
        sys.exit(1)
    if alg_input.forecast_aggregation not in (None, "last", "maximum"):
        print("forecastAggregation must be last or maximum", file=sys.stderr)
        sys.exit(1)
    if alg_input.objective not in (None, "reg:squarederror", "reg:squaredlogerror", "reg:absoluteerror",
                                    "reg:pseudohubererror", "count:poisson", "reg:gamma", "reg:tweedie", "reg:quantileerror", "survival:aft"):
        print("Unsupported CPU regression objective", file=sys.stderr)
        sys.exit(1)
    quantile=alg_input.objective=='reg:quantileerror'
    aft=alg_input.objective=='survival:aft'
    scale=alg_input.aft_loss_distribution_scale
    threshold=alg_input.cpu_censoring_utilization_threshold
    if aft:
        if alg_input.forecast_strategy!='direct' or alg_input.metric_history is None:
            raise ValueError('survival:aft requires direct forecasting and CPU utilization history')
        if type(scale) not in (int,float) or not math.isfinite(scale) or not .05<=scale<=2:
            raise ValueError('aftLossDistributionScale must be finite and in [0.05, 2]')
        if type(threshold) not in (int,float) or not math.isfinite(threshold) or not 90<=threshold<=10000:
            raise ValueError('cpuCensoringUtilizationThreshold must be finite and in [90, 10000]')
    elif scale is not None or threshold is not None:
        raise ValueError('AFT parameters require objective survival:aft')
    alpha=alg_input.quantile_alpha
    if quantile != (alpha is not None) or (alpha is not None and (type(alpha) not in (int,float) or not math.isfinite(alpha) or not 0 < alpha < 1)):
        print('reg:quantileerror requires one finite quantileAlpha strictly between zero and one; other objectives must omit it', file=sys.stderr)
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
    if alg_input.window_size is not None and not 0 < alg_input.window_size <= alg_input.lags:
        print("windowSize must be > 0 and <= lags", file=sys.stderr)
        sys.exit(1)
    for name in ("max_depth", "n_estimators", "learning_rate"):
        value = getattr(alg_input, name)
        if value is not None and (not math.isfinite(value) or value <= 0):
            print(f"{name} must be finite and > 0", file=sys.stderr)
            sys.exit(1)
    for name in ("subsample", "colsample_bytree"):
        value = getattr(alg_input, name)
        if value is not None and not 0 < value <= 1:
            print(f"{name} must be in (0, 1]", file=sys.stderr)
            sys.exit(1)
    for name in ("min_child_weight", "gamma", "reg_lambda", "reg_alpha"):
        value = getattr(alg_input, name)
        if value is not None and (not math.isfinite(value) or value < 0):
            print(f"{name} must be finite and >= 0", file=sys.stderr)
            sys.exit(1)
    if alg_input.metric_history is not None and len(alg_input.metric_history) != len(alg_input.replica_history):
        print("metricHistory must match length of replicaHistory when provided", file=sys.stderr)
        sys.exit(1)


def sort_history(replica_history: List[TimestampedReplica],
                 metric_history: Optional[List[float]]) -> Tuple[List[TimestampedReplica], Optional[List[float]]]:
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
                        scope: Optional[str] = None, seasonal: bool = False, direct: bool = False,
                        time_features: bool = False) -> None:
    if os.environ.get("HPAPLUS_FEATURE_DUMP_DIR") and scope:
        dump_features("xgboost", scope, feature_header(rolling_window, metrics_used, seasonal, direct, time_features), X_train)


def build_model_params(alg_input: AlgorithmInput) -> dict:
    """Apply defaults only to omitted values, preserving explicit zeroes."""
    defaults = {
        "n_estimators": 200, "max_depth": 4, "learning_rate": 0.1,
        "subsample": 0.8, "colsample_bytree": 1.0,
        "objective": "reg:squarederror", "min_child_weight": 1.0,
        "gamma": 0.0, "reg_lambda": 1.0, "reg_alpha": 0.0,
    }
    params = {name: default if getattr(alg_input, name) is None else getattr(alg_input, name)
              for name, default in defaults.items()}
    params.update(n_jobs=1, verbosity=0)
    if alg_input.objective=='reg:quantileerror':
        params.update(quantile_alpha=alg_input.quantile_alpha,tree_method='hist')
    return params


def cpu_demand_weights(targets, exponent):
    """Bound loss emphasis on larger observed CPU targets without changing them."""
    if exponent is None:
        return None
    if type(exponent) not in (int, float) or not math.isfinite(exponent) or not 0 <= exponent <= 2:
        raise ValueError("cpuDemandWeightExponent must be finite and in [0, 2]")
    if exponent == 0:
        return None
    values = np.asarray(targets, dtype=float)
    if values.ndim != 1 or values.size == 0 or not np.all(np.isfinite(values)) or np.any(values < 0):
        raise ValueError("CPU weighting requires finite nonnegative training targets")
    median = max(1., float(np.median(values)))
    return np.clip((np.maximum(values, 1.) / median) ** exponent, .5, 4.)


def main() -> None:
    alg_input = read_input()
    validate_input(alg_input)
    direct = alg_input.forecast_strategy == "direct"
    aft = alg_input.objective == 'survival:aft'
    if type(alg_input.return_trajectory) is not bool:
        raise ValueError("returnTrajectory must be boolean")
    tree_budget("XGBoost", len(alg_input.replica_history), alg_input.lags,
                alg_input.window_size, alg_input.metric_history is not None, alg_input.n_estimators, alg_input.max_depth,
                None, (3 if alg_input.seasonal_features else 0) + int(direct)
                + (len(trend_feature_header(alg_input.window_size or alg_input.lags)) if direct else 0))

    sorted_history, sorted_metrics = sort_history(alg_input.replica_history, alg_input.metric_history)
    steps = determine_steps(alg_input.look_ahead, sorted_history)
    series = extract_cpu_usage_series(sorted_history)
    if alg_input.return_trajectory and steps > 32:
        raise ValueError("timed forecast exceeds the 32-point budget")

    # Fallback: not enough data to train
    if len(series) <= alg_input.lags:
        emit_prediction(series[-1])
        return

    rolling_window = alg_input.window_size or alg_input.lags
    rolling_window = max(1, min(rolling_window, alg_input.lags))

    training_phase, future_phase = None, None
    if alg_input.seasonal_features or direct or alg_input.return_trajectory:
        if any(item.time is None for item in sorted_history):
            raise ValueError("clock forecasting requires timestamps")
        stamps = [parse_time(item.time) for item in sorted_history]
        if len(stamps) < 2 or any(b <= a for a,b in zip(stamps,stamps[1:])):
            raise ValueError("clock forecasting requires increasing timestamps")
        times = [float(t-stamps[0]) for t in stamps]
        interval = max(.001, float(statistics.median(b-a for a,b in zip(stamps,stamps[1:]))))
        if alg_input.seasonal_features:
            period = estimate_period(times, series)
            training_phase = phase_features(times, 0., period)
            future_phase = phase_features([times[-1]+s*interval for s in range(1,steps+1)], 0., period)
    if direct:
        target_options = {"return_target_indices": True} if aft else {}
        matrix_result = direct_matrices(series, sorted_metrics, alg_input.lags,
            rolling_window, times, interval, steps, training_phase, future_phase,
            alg_input.n_estimators, alg_input.max_depth, time_features=True, **target_options)
        X_train, y_train, direct_features = matrix_result[:3]
        if aft:
            target_utilizations = np.asarray(sorted_metrics)[matrix_result[3]]
    else:
        X_train, y_train = build_training_matrix(series, sorted_metrics, alg_input.lags, rolling_window, training_phase)
    if X_train.size == 0 or y_train.size == 0:
        emit_prediction(series[-1])
        return

    maybe_dump_features(X_train, rolling_window, sorted_metrics is not None, alg_input.diagnostic_scope, bool(alg_input.seasonal_features), direct, direct)

    with stage("training"):
        weights = cpu_demand_weights(y_train, alg_input.cpu_demand_weight_exponent)
        if aft:
            model = fit_cpu_aft(xgb, X_train, y_train, target_utilizations,
                build_model_params(alg_input), alg_input.aft_loss_distribution_scale,
                alg_input.cpu_censoring_utilization_threshold, weights)
        else:
            model = xgb.XGBRegressor(**build_model_params(alg_input))
            if weights is None:
                model.fit(X_train, y_train)
            else:
                model.fit(X_train, y_train, sample_weight=weights)

    with stage("inference"):
        if direct:
            prediction = direct_forecast(model, direct_features, alg_input.forecast_aggregation or "last", alg_input.return_trajectory)
        else:
            prediction = forecast(model, series, sorted_metrics, alg_input.lags, steps, rolling_window,
                                  alg_input.forecast_aggregation or "last", future_phase, return_values=alg_input.return_trajectory)
    if alg_input.return_trajectory:
        from forecast_trajectory import result
        points = result(0, prediction, [r.time for r in sorted_history], alg_input.look_ahead)["trajectory"]
        scalar = max(p["cpu"] for p in points) if alg_input.forecast_aggregation == "maximum" else points[-1]["cpu"]
        sys.stdout.write(json.dumps({"prediction": scalar, "trajectory": points}, separators=(",", ":")))
        return
    clamped = max(0, int(round(prediction)))
    emit_prediction(clamped)


if __name__ == "__main__":
    main()
