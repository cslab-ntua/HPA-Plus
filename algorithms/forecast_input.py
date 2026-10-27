"""Validate versioned CPU training data before model libraries coerce JSON values.

The controller also validates its own histories. This boundary protects direct
versioned callers and prevents legacy CLI warmup fallbacks from masquerading as
successful production forecasts. Check tuning types here before dataclasses or
numerical libraries can silently coerce JSON booleans, strings or fractions.
"""
import math

from forecast_time import parse_timestamp, forecast_steps, holt_forecast_steps

MAX_CPU = 2**63 - 1
MAX_DURATION_MS = (2**63 - 1) // 1000000


def integer(value, name, minimum=0, maximum=MAX_CPU):
    if type(value) is not int or not minimum <= value <= maximum:
        raise ValueError("%s must be an integer in [%d, %d]" % (name, minimum, maximum))
    return value


def number(value, name):
    if type(value) not in (int, float) or not math.isfinite(value):
        raise ValueError("%s must be a finite number" % name)
    return value


def validate_tree_configuration(model, config):
    """Keep optional null/defaults and explicit zero regularization semantics."""
    if model == "XGBoost" and config.get("forecastStrategy") not in (None, "recursive", "direct"):
        raise ValueError("forecastStrategy must be recursive or direct")
    if model == "XGBoost" and config.get("forecastAggregation") not in (None, "last", "maximum"):
        raise ValueError("forecastAggregation must be last or maximum")
    if model == "XGBoost" and config.get("seasonalFeatures") is not None and type(config["seasonalFeatures"]) is not bool:
        raise ValueError("seasonalFeatures must be boolean")
    if model == "XGBoost" and any(k in config for k in ("adaptiveSeasonality", "seasonalPeriodHintSeconds")):
        raise ValueError("cycle-shift settings have been removed; use model.forecastReliability")
    integers = {"windowSize": 1, "maxDepth": 1, "nEstimators": 1}
    if model == "LightGBM":
        integers.update(numLeaves=2, minChildSamples=1)
    for name, minimum in integers.items():
        value = config.get(name)
        if value is None:
            continue
        if model == "LightGBM" and name == "maxDepth" and type(value) is int and value == -1:
            continue
        integer(value, name, minimum)
    if config.get("windowSize") is not None and config["windowSize"] > config["lags"]:
        raise ValueError("windowSize must not exceed lags")
    for name in ("learningRate", "subsample", "colsampleBytree", "regLambda", "regAlpha", "minChildWeight", "gamma"):
        if name in ("minChildWeight", "gamma") and model != "XGBoost":
            continue
        value = config.get(name)
        if value is None:
            continue
        number(value, name)
        if name in ("learningRate", "subsample", "colsampleBytree"):
            if value <= 0 or (name != "learningRate" and value > 1):
                raise ValueError("%s is outside its supported range" % name)
        elif value < 0:
            raise ValueError("%s must be nonnegative" % name)
    objectives = ("reg:squarederror", "reg:squaredlogerror", "reg:absoluteerror", "reg:pseudohubererror",
                  "count:poisson", "reg:gamma", "reg:tweedie", "reg:quantileerror", "survival:aft") if model == "XGBoost" else (
                      "regression", "regression_l2", "l2", "mean_squared_error", "mse", "regression_l1", "l1",
                      "mean_absolute_error", "mae", "huber", "fair", "poisson", "quantile", "mape",
                      "mean_absolute_percentage_error", "gamma", "tweedie")
    if config.get("objective") is not None and config["objective"] not in objectives:
        raise ValueError("unsupported CPU regression objective")
    if model == "XGBoost":
        aft=config.get('objective')=='survival:aft'
        scale=config.get('aftLossDistributionScale')
        threshold=config.get('cpuCensoringUtilizationThreshold')
        if aft:
            if config.get('forecastStrategy')!='direct' or config.get('metricHistory') is None:
                raise ValueError('survival:aft requires direct forecasting and CPU utilization history')
            if not .05<=number(scale,'aftLossDistributionScale')<=2:
                raise ValueError('aftLossDistributionScale must be in [0.05, 2]')
            if not 90<=number(threshold,'cpuCensoringUtilizationThreshold')<=10000:
                raise ValueError('cpuCensoringUtilizationThreshold must be in [90, 10000]')
        elif scale is not None or threshold is not None:
            raise ValueError('AFT parameters require objective survival:aft')
        power = config.get('cpuDemandWeightExponent')
        if power is not None and not 0 <= number(power, 'cpuDemandWeightExponent') <= 2:
            raise ValueError('cpuDemandWeightExponent must be in [0, 2]')
        alpha=config.get('quantileAlpha')
        quantile=config.get('objective')=='reg:quantileerror'
        if quantile != (alpha is not None):
            raise ValueError('reg:quantileerror requires quantileAlpha; other objectives must omit it')
        if alpha is not None and not 0 < number(alpha,'quantileAlpha') < 1:
            raise ValueError('quantileAlpha must be strictly between zero and one')


def validate_holt_winters_configuration(config):
    for name in ("alpha", "beta", "gamma"):
        if not 0 <= number(config.get(name), name) <= 1:
            raise ValueError("%s must be in [0, 1]" % name)
    for name in ("trend", "seasonal"):
        if config.get(name) not in ("add", "additive", "mul", "multiplicative"):
            raise ValueError("invalid Holt-Winters %s" % name)
    if "dampedTrend" in config and type(config["dampedTrend"]) is not bool:
        raise ValueError("dampedTrend must be a boolean")
    method = config.get("initializationMethod", "estimated")
    if method not in ("estimated", "heuristic", "known", "legacy-heuristic"):
        raise ValueError("invalid initializationMethod")
    for name in ("initialLevel", "initialTrend", "initialSeasonal"):
        value = config.get(name)
        if method == "known":
            number(value, name)
        elif value is not None:
            raise ValueError("initial values require initializationMethod known")
    if any(config[name] in ("mul", "multiplicative") for name in ("trend", "seasonal")):
        if any(value <= 0 for value in config["series"]):
            raise ValueError("multiplicative Holt-Winters requires strictly positive CPU history")


def metric(value):
    if type(value) not in (int, float) or not math.isfinite(value) or value < 0:
        raise ValueError("CPU utilization must be a finite nonnegative number")


def validate_training_input(model, payload):
    if "returnTrajectory" in payload and type(payload["returnTrajectory"]) is not bool:
        raise ValueError("returnTrajectory must be boolean")
    if payload.get("returnTrajectory") is True:
        from forecast_trajectory import grid
        timestamps = payload.get("timestamps") if model == "HoltWinters" else [
            row.get("time") for row in payload.get("replicaHistory", []) if isinstance(row, dict)]
        grid(timestamps or [], payload.get("lookAhead"))
    if model == "HoltWinters":
        period = integer(payload.get("seasonalPeriods"), "seasonalPeriods", 2)
        series = payload.get("series")
        required = max(2*period, 10+2*(period//2))
        if not isinstance(series, list) or len(series) < required:
            raise ValueError("insufficient CPU history for Holt-Winters initialization")
        for value in series:
            integer(value, "CPU millicores")
        if payload.get("lookAhead") is not None:
            integer(payload["lookAhead"], "lookAhead", 1, MAX_DURATION_MS)
        holt_forecast_steps(payload.get("lookAhead"), payload.get("timestamps"), len(series))
        return

    integer(payload.get("lookAhead"), "lookAhead", 1, MAX_DURATION_MS)
    required = 2 if model == "Linear" else 3
    if model in ("XGBoost", "LightGBM"):
        required = integer(payload.get("lags"), "lags", 1) + 1
    history = payload.get("replicaHistory")
    if not isinstance(history, list) or len(history) < required:
        raise ValueError("insufficient CPU history for prediction")
    timestamps = set()
    for sample in history:
        if not isinstance(sample, dict):
            raise ValueError("CPU history samples must be objects")
        integer(sample.get("totalCpuUsageMillicores"), "CPU millicores")
        integer(sample.get("replicas"), "observed replicas", 0, 2**31 - 1)
        stamp = sample.get("time")
        # Preserve the existing ARIMA one-shot compatibility path: if any
        # timestamps are absent it keeps input order and estimates a 1s interval.
        if stamp is not None or model != "ARIMA":
            parsed = parse_timestamp(stamp)
            if parsed in timestamps:
                raise ValueError("CPU history timestamps must be distinct")
            timestamps.add(parsed)
        if sample.get("metric") is not None:
            metric(sample["metric"])

    auxiliary = payload.get("metricHistory")
    if auxiliary is not None:
        if not isinstance(auxiliary, list) or len(auxiliary) != len(history):
            raise ValueError("metricHistory must match replicaHistory")
        for value in auxiliary:
            metric(value)
    if model == "Linear":
        if parse_timestamp(payload.get("currentTime")) != max(timestamps):
            raise ValueError("Linear currentTime must match the latest CPU sample")
    else:
        # Match ARIMA's legacy missing-time fallback and the models' sorted
        # interval estimate. Reject before loading any numerical model code.
        forecast_steps(payload["lookAhead"], sorted(timestamps) if len(timestamps) == len(history) else [])


def validate_arima_configuration(config, incremental=False):
    """Check ARIMA types before dataclass/bool/int coercion or worker mutation."""
    if not isinstance(config, dict):
        raise ValueError("ARIMA configuration must be an object")
    integer(config.get("lookAhead"), "lookAhead", 1, MAX_DURATION_MS)
    if "returnTrajectory" in config and type(config["returnTrajectory"]) is not bool:
        raise ValueError("returnTrajectory must be boolean")
    for name in ("order", "maxOrder", "seasonalOrder"):
        values = config.get(name)
        if values is None and name != "order":
            continue
        if not isinstance(values, list) or len(values) != 3:
            raise ValueError("%s must contain three nonnegative integers" % name)
        for value in values:
            integer(value, name)
    for name in ("autoArima", "enforceStationarity", "enforceInvertibility", "concentrateScale", "useSarima"):
        if name in config and type(config[name]) is not bool:
            raise ValueError("%s must be a boolean" % name)
    trend = config.get("trend")
    if trend not in (None, "n", "c", "t", "ct"):
        raise ValueError("invalid ARIMA trend")
    if config.get("informationCriterion", "aic") not in ("aic", "bic"):
        raise ValueError("informationCriterion must be aic or bic")
    if config.get("seasonalPeriods") is not None:
        integer(config["seasonalPeriods"], "seasonalPeriods", 2)
    seasonal = config.get("useSarima", False) or config.get("seasonalOrder") is not None or config.get("seasonalPeriods") is not None
    automatic = config.get("autoArima", False)
    if automatic and (incremental or seasonal):
        raise ValueError("autoArima supports non-seasonal one-shot execution only")
    if seasonal and (config.get("seasonalOrder") is None or config.get("seasonalPeriods") is None):
        raise ValueError("seasonal ARIMA requires seasonalOrder and seasonalPeriods")
    if automatic and config.get("maxOrder") is not None:
        if any(value > bound for value, bound in zip(config["order"], config["maxOrder"])):
            raise ValueError("order must not exceed maxOrder during auto selection")
    if not seasonal and not automatic:
        difference = config["order"][1]
        if (difference > 0 and trend in ("c", "ct")) or (difference > 1 and trend == "t"):
            raise ValueError("ARIMA trend degree must be at least the differencing order")
