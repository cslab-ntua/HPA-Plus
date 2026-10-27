"""Deterministic work ceilings, checked before training allocations.

These are workload proxies, not a promise about library RSS or CPU seconds.
Process deadlines and container memory limits remain necessary.
"""
MAX_TRAINING_ROWS = 10000
MAX_MATRIX_CELLS = 1000000
MAX_ESTIMATORS = 2000
MAX_TREE_DEPTH = 16
MAX_TREE_LEAVES = 65536
MAX_TREE_WORK = 200000000
MAX_ARIMA_CANDIDATES = 256
MAX_ARIMA_STATES = 128
MAX_ARIMA_STATE_CELLS = 2000000


def limit(value, maximum, name):
    if value > maximum:
        raise ValueError("training budget exceeded: %s (%d > %d)" % (name, value, maximum))


def training_rows(count):
    limit(count, MAX_TRAINING_ROWS, "history rows")


def training_matrix(count, lags, window, metrics_used, extra_features=0):
    training_rows(count)
    if type(extra_features) is not int or not 0 <= extra_features <= 16:
        raise ValueError("invalid extra feature count")
    width = window + max(1, window - 1) + 2
    if metrics_used:
        width += window + 2
    width += extra_features
    # Include an inference row even for the legacy warmup path.
    cells = max(1, count - lags) * width
    limit(cells, MAX_MATRIX_CELLS, "feature matrix cells")
    return cells


def tree_budget(model, count, lags, window, metrics_used, estimators=None, depth=None, leaves=None, extra_features=0):
    cells = training_matrix(count, lags, window or lags, metrics_used, extra_features)
    estimators = 200 if estimators is None else estimators
    depth = 4 if depth is None else depth
    limit(estimators, MAX_ESTIMATORS, "estimators")
    limit(depth, MAX_TREE_DEPTH, "tree depth")
    if model == "LightGBM":
        limit(31 if leaves is None else leaves, MAX_TREE_LEAVES, "tree leaves")
    limit(cells * estimators, MAX_TREE_WORK, "tree training work")


def arima_budget(count, order, seasonal_order=None, seasonal_periods=None, max_order=None):
    training_rows(count)
    if max_order is not None:
        p, d, q = max_order
        # Baseline plus the grid, whose empty order is skipped, uses at most
        # this many fits. Include the final selected-model fit separately.
        limit((p + 1) * (d + 1) * (q + 1) + 1, MAX_ARIMA_CANDIDATES, "ARIMA fits")
        order = [max(a, b) for a, b in zip(order, max_order)]
    p, d, q = order
    sp, sd, sq = seasonal_order or (0, 0, 0)
    period = seasonal_periods or 0
    states = max(p + period * sp, q + period * sq + 1) + d + period * sd
    limit(states, MAX_ARIMA_STATES, "ARIMA states")
    limit(count * states * states, MAX_ARIMA_STATE_CELLS, "ARIMA state cells")


def request_budget(model, payload):
    count = len(payload.get("series" if model == "HoltWinters" else "replicaHistory", []))
    training_rows(count)
    if model in ("XGBoost", "LightGBM"):
        metrics = payload.get("metricHistory") is not None
        time_features=0
        if model=="XGBoost" and payload.get("forecastStrategy")=="direct":
            from forecast_trends import trend_feature_header
            time_features=len(trend_feature_header(payload.get("windowSize") or payload["lags"]))
        tree_budget(model, count, payload["lags"], payload.get("windowSize"), metrics,
                    payload.get("nEstimators"), payload.get("maxDepth"), payload.get("numLeaves"),
                    ((3 if payload.get("seasonalFeatures") else 0) + (1 if payload.get("forecastStrategy") == "direct" else 0) + time_features) if model == "XGBoost" else 0)
    elif model == "ARIMA":
        maximum = None
        if payload.get("autoArima"):
            maximum = payload.get("maxOrder") or [max(a, b) for a, b in zip(payload["order"], (5, 2, 5))]
        arima_budget(count, payload["order"], payload.get("seasonalOrder"), payload.get("seasonalPeriods"), maximum)
