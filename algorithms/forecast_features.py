"""Shared CPU lag features and recursive inference for both tree forecasters."""
from collections import deque
import math

import numpy as np
from forecast_budget import training_matrix
from forecast_trends import trend_feature_header


def build_feature_row(cpu_window, metric_window):
    row = list(cpu_window)
    if len(cpu_window) > 1:
        row.extend(np.diff(cpu_window).tolist())
    else:
        row.append(0.0)
    row.extend((float(np.mean(cpu_window)), float(np.std(cpu_window))))
    if metric_window:
        row.extend(metric_window)
        row.extend((float(np.mean(metric_window)), float(np.std(metric_window))))
    return row


def feature_header(window, metrics_used, seasonal=False, direct=False,
                   time_features=False):
    if type(time_features) is not bool:
        raise ValueError('CPU trend options must be boolean')
    if time_features and not direct:
        raise ValueError('CPU time trends require clock-based direct forecasting')
    # A one-sample window still contains a zero difference feature.
    header = ["cpu_lag_%d" % i for i in range(window)]
    header += ["cpu_diff_%d" % i for i in range(max(1, window-1))]
    header += ["cpu_mean", "cpu_std"]
    if metrics_used:
        header += ["metric_lag_%d" % i for i in range(window)]
        header += ["metric_mean", "metric_std"]
    if seasonal:
        header += ["target_cycle_phase", "target_cycle_sin", "target_cycle_cos"]
    if direct:
        header += ["forecast_lead_intervals"]
    if time_features:
        header += trend_feature_header(window)
    return header


def build_training_matrix(series, metrics, lags, rolling_window, target_features=None):
    extra = len(target_features[0]) if target_features is not None else 0
    training_matrix(len(series), lags, rolling_window, metrics is not None, extra)
    features, targets = [], []
    for index in range(lags, len(series)):
        # Features stop before the target; auxiliary data follows the same slice.
        cpu_window = series[index-lags:index][-rolling_window:]
        metric_window = metrics[index-lags:index][-rolling_window:] if metrics else None
        row = build_feature_row(cpu_window, metric_window)
        if target_features is not None:
            row.extend(target_features[index])
        features.append(row)
        targets.append(series[index])
    return np.array(features), np.array(targets)


def forecast(model, history, metrics, lags, steps, rolling_window, aggregation="last", future_features=None, return_values=False):
    if aggregation not in ("last", "maximum"):
        raise ValueError("forecast aggregation must be last or maximum")
    # Only the lag window is needed across steps. Avoid retaining every predicted
    # value for long horizons; inputs remain owned by the caller.
    cpu_series = deque(history[-lags:], maxlen=lags)
    metric_series = deque(metrics[-lags:], maxlen=lags) if metrics else None
    peak = None
    values = [] if return_values else None
    for step in range(steps):
        if len(cpu_series) < lags:
            return cpu_series[-1]  # Legacy helper compatibility; v1 rejects warmup.
        cpu_window = list(cpu_series)[-rolling_window:]
        metric_window = list(metric_series)[-rolling_window:] if metric_series else None
        row = build_feature_row(cpu_window, metric_window)
        if future_features is not None:
            row.extend(future_features[step])
        features = np.array([row])
        next_value = float(model.predict(features)[0])
        if not math.isfinite(next_value) or not -(2**63) <= next_value < 2**63:
            raise ValueError("tree forecast step must contain finite int64 CPU millicores")
        cpu_series.append(next_value)
        if values is not None:
            values.append(next_value)
        peak = next_value if peak is None else max(peak, next_value)
        if metric_series is not None:
            metric_series.append(metric_series[-1])  # Hold the last observed auxiliary metric.
    if values is not None:
        return values
    return peak if aggregation == "maximum" and peak is not None else cpu_series[-1]
