#!/usr/bin/env python3
"""Causal direct XGBoost forecasts of measured incoming requests/second.

The target is a completed accounting interval, not a generator schedule. Feature
receipts are checked at every historical decision time. Future inference uses
only real lag windows; it never fabricates future exogenous observations.
"""
from datetime import datetime, timezone
from decimal import ROUND_CEILING
import json
import math
import sys

import numpy as np

from forecast_seasonality import estimate_period, phase_features
from forecast_time import parse_timestamp

PROTOCOL = "request-demand-v1"
MAX_OBSERVATIONS = 4096
MAX_FEATURES = 32
MAX_MATRIX_CELLS = 1000000


def stamp(seconds):
    return datetime.fromtimestamp(float(seconds), timezone.utc).isoformat().replace("+00:00", "Z")


def finite(value, name, minimum=None):
    if type(value) not in (int, float) or not math.isfinite(value):
        raise ValueError(name + " must be a finite number")
    if minimum is not None and value < minimum:
        raise ValueError(name + " is below its minimum")
    return float(value)


def integer(value, name, minimum, maximum):
    if type(value) is not int or not minimum <= value <= maximum:
        raise ValueError(name + " is outside its integer bounds")
    return value


def timestamp(value):
    return parse_timestamp(value)


def ceiling_interval(value, interval):
    return (value / interval).to_integral_value(rounding=ROUND_CEILING) * interval


def parse_request(payload):
    if payload.get("protocol") != PROTOCOL:
        raise ValueError("unsupported request-demand protocol")
    config = payload.get("config", {})
    interval = integer(config.get("intervalSeconds", 30), "intervalSeconds", 1, 300)
    history_size = integer(config.get("historySize", 60), "historySize", 24, 240)
    lags = integer(config.get("lags", 12), "lags", 1, 48)
    steps = integer(config.get("horizonSteps", 3), "horizonSteps", 1, 20)
    if lags + steps + 8 > history_size:
        raise ValueError("historySize must provide at least eight direct training targets")
    alpha = finite(config.get("quantile", .95), "quantile")
    if not 0 < alpha < 1:
        raise ValueError("quantile must be strictly between zero and one")
    seasonal = config.get("seasonalFeatures", True)
    if type(seasonal) is not bool:
        raise ValueError("seasonalFeatures must be boolean")
    estimators = integer(config.get("nEstimators", 300), "nEstimators", 1, 2000)
    depth = integer(config.get("maxDepth", 5), "maxDepth", 1, 16)
    parameters = {}
    for name, key, default, positive, maximum in (
            ("learningRate", "learning_rate", .1, True, 1.),
            ("subsample", "subsample", .8, True, 1.),
            ("colsampleBytree", "colsample_bytree", 1., True, 1.),
            ("minChildWeight", "min_child_weight", 1., False, 1e6),
            ("gamma", "gamma", 0., False, 1e6),
            ("regLambda", "reg_lambda", 1., False, 1e6),
            ("regAlpha", "reg_alpha", 0., False, 1e6)):
        value = finite(config.get(name, default), name, 0)
        if positive and value == 0 or value > maximum:
            raise ValueError(name + " is outside its supported bounds")
        parameters[key] = value
    requested = timestamp(payload["requestedAt"])
    forecast_interval = integer(config.get("forecastIntervalSeconds") or interval, "forecastIntervalSeconds", 1, 300)
    if interval % forecast_interval:
        raise ValueError("forecast interval must divide the history cadence")
    first = timestamp(payload["firstForecastAt"])
    if first < requested or first % forecast_interval != 0:
        raise ValueError("firstForecastAt must be a future UTC interval boundary")
    specs = config.get("features", [])
    if not isinstance(specs, list) or len(specs) > MAX_FEATURES:
        raise ValueError("features exceed the supported feature budget")
    names = set()
    for spec in specs:
        name, unit = spec.get("name"), spec.get("unit")
        if not isinstance(name, str) or not name or len(name) > 128 or name in names:
            raise ValueError("feature names must be unique nonempty strings")
        if not isinstance(unit, str) or not unit or len(unit) > 64:
            raise ValueError("feature unit must be a nonempty string")
        names.add(name)
        integer(spec.get("maxAgeSeconds", 90), "maxAgeSeconds", 1, 3600)
    specs = sorted(specs, key=lambda item: item["name"])
    raw = payload.get("observations", [])
    if not isinstance(raw, list) or len(raw) > MAX_OBSERVATIONS:
        raise ValueError("observations exceed the input budget")
    observations, slots, fine_targets = [], set(), {}
    for item in raw:
        at, until, receipt = (timestamp(item[key]) for key in ("at", "until", "receivedAt"))
        if until - at != interval or at % interval != 0 or receipt < until:
            raise ValueError("observations require aligned complete intervals and honest receipts")
        if at in slots:
            raise ValueError("duplicate accounting interval")
        slots.add(at)
        value = item.get("arrivalsPerSecond")
        if value is not None:
            value = finite(value, "arrivalsPerSecond", 0)
        coverage = item.get("arrivalCoverage")
        if coverage is not None:
            coverage = finite(coverage, "arrivalCoverage", 0)
            if coverage > 1 or value is not None and coverage < .95:
                raise ValueError("genuine arrival labels require at least 95% coverage")
        if type(item.get("arrivalBoundaryAveraged", False)) is not bool:
            raise ValueError("arrivalBoundaryAveraged must be boolean")
        subintervals = item.get("arrivalIntervals", [])
        if not isinstance(subintervals, list) or len(subintervals) > interval // forecast_interval:
            raise ValueError("too many arrival subintervals")
        seen_fine = set()
        for point in subintervals:
            begin, end, available = (timestamp(point[key]) for key in ("at", "until", "receivedAt"))
            if (begin < at or end > until or end-begin != forecast_interval or begin % forecast_interval
                    or available < end or available > receipt or begin in seen_fine):
                raise ValueError("invalid, duplicate or backdated arrival subinterval")
            seen_fine.add(begin)
            coverage = finite(point.get("coverage"), "subinterval coverage", 0)
            if coverage > 1 or type(point.get("boundaryAveraged", False)) is not bool:
                raise ValueError("invalid arrival subinterval coverage")
            rate = point.get("rate")
            if rate is not None:
                rate = finite(rate, "subinterval rate", 0)
                if coverage < .95:
                    raise ValueError("genuine subinterval requires 95% coverage")
            if available <= requested and receipt <= requested and end <= requested:
                fine_targets[begin] = dict(at=begin, until=end, receipt=available, rate=rate)
        features = item.get("features") or {}
        if not isinstance(features, dict):
            raise ValueError("observation features must be an object")
        # Future or incomplete observations cannot become training labels.
        if receipt <= requested and until <= requested:
            observations.append(dict(at=at, until=until, receipt=receipt,
                                     rate=value, features=features))
    observations.sort(key=lambda item: item["at"])
    return dict(forecast_interval=forecast_interval, fine_targets=fine_targets, interval=interval, history_size=history_size, lags=lags, steps=steps,
                alpha=alpha, seasonal=seasonal, estimators=estimators, depth=depth, parameters=parameters,
                requested=requested, requested_stamp=payload["requestedAt"], first=first, specs=specs, observations=observations)


def feature_metadata(feature, item, spec):
    """Keep native read time separate from the interval its value measures."""
    if not isinstance(feature, dict):
        raise ValueError("feature value must be an object")
    source, receipt = timestamp(feature["sourceAt"]), timestamp(feature["receivedAt"])
    if receipt < source:
        raise ValueError("feature receipt precedes its native read")
    if feature.get("unit") != spec["unit"]:
        raise ValueError("feature unit differs from its declared schema")
    window = feature.get("measurementWindow")
    if window is not None:
        if not isinstance(window, dict) or "from" not in window or "to" not in window:
            raise ValueError("feature measurement window must declare from and to")
        begin, end = timestamp(window["from"]), timestamp(window["to"])
        if begin >= end or end > item["until"] or receipt < end:
            raise ValueError("feature measurement window contradicts its accounting interval")
        age = item["until"]-end
    else:
        if source > item["until"]:
            raise ValueError("instant feature source is after its accounting interval")
        age = item["until"]-source
    raw_value = feature.get("value")
    if raw_value is not None:
        raw_value = finite(raw_value, "feature value")
    return source, receipt, float(age), raw_value


def feature_window(observations, specs, anchor_time, metadata_cache=None):
    """Values/masks/ages for past slots; no forward-fill or future receipts."""
    row = []
    for spec in specs:
        for item in observations:
            feature = item["features"].get(spec["name"])
            value, missing, age = np.nan, 1., np.nan
            if feature is not None:
                key = (item["at"], spec["name"])
                metadata = None if metadata_cache is None else metadata_cache.get(key)
                if metadata is None:
                    metadata = feature_metadata(feature, item, spec)
                    if metadata_cache is not None:
                        metadata_cache[key] = metadata
                source, receipt, measured_age, raw_value = metadata
                if receipt <= anchor_time and source <= anchor_time:
                    # A historical lag remains a historical measurement. Native
                    # reads can end later than the integrated measurement window;
                    # availability uses the honest receipt, freshness its window.
                    age = measured_age
                    if raw_value is not None and age <= spec.get("maxAgeSeconds", 90):
                        value, missing = raw_value, 0.
            row.extend((value, missing, age))
    return row


def arrival_features(slots, cutoff):
    """Keep the fixed lag grid with native NaNs, availability masks and age."""
    values = np.asarray([np.nan if item["rate"] is None else item["rate"] for item in slots], dtype=float)
    present = np.isfinite(values)
    result = values.tolist()
    result.extend(np.diff(values).tolist() if len(values) > 1 else [0.])
    result.extend((float(np.mean(values[present])), float(np.std(values[present]))))
    result.extend((~present).astype(float).tolist())
    result.extend(float(cutoff-item["until"]) if known else np.nan for item, known in zip(slots, present))
    return result


def build_matrices(config):
    """Training rows each have an explicit historical publication cutoff."""
    observed = config["observations"]
    valid = [item for item in observed if item["rate"] is not None]
    if len(valid) < config["history_size"]:
        return None, "need %d genuine arrival samples; have %d" % (config["history_size"], len(valid))
    # Keep actual accounting slots, including missing ones. A warm-up count is
    # never achieved by copying values into missing slots.
    oldest = valid[-config["history_size"]]["at"]
    observed = [item for item in observed if item["at"] >= oldest]
    # The seasonal basis is a parameter of this fit. Estimate it once using
    # genuine target intervals available at this invocation, then encode every
    # training row and future point in that same coordinate system. Re-estimating
    # it at each historical anchor gives the same phase feature different
    # meanings. Raw lag values still obey their original historical receipts.
    seasonal_origin = float(observed[0]["at"])
    by_at = {item["at"]: item for item in observed}
    forecast_interval = config["forecast_interval"]
    targets = config["fine_targets"] if forecast_interval != config["interval"] else by_at
    basis_rows = sorted((item for item in targets.values()
                         if item["at"] >= oldest and item["rate"] is not None
                         and item["until"] <= config["requested"]
                         and item["receipt"] <= config["requested"]), key=lambda item: item["at"])
    basis_period = estimate_period([float(item["at"]) for item in basis_rows],
                                   [item["rate"] for item in basis_rows]) if config["seasonal"] else None
    interval, lags = config["interval"], config["lags"]
    latest = valid[-1]
    if config["requested"] - latest["until"] > 2 * interval:
        return None, "arrival observations are stale"

    def lag_slots(anchor, cutoff):
        slots = []
        for i in range(lags-1, -1, -1):
            at = anchor["at"]-i*interval
            item = by_at.get(at)
            if item is None:
                slots.append(dict(at=at, until=at+interval, rate=None, features={}))
            else:
                known = item["rate"] is not None and item["receipt"] <= cutoff
                slots.append(dict(item, rate=item["rate"] if known else None))
        if sum(item["rate"] is not None for item in slots) < max(1, math.ceil(lags/2)):
            return None
        return slots

    # Cache immutable parsed metadata within this invocation only. Availability
    # is still evaluated separately at every original historical cutoff.
    metadata_cache = {}

    def base_row(slots, cutoff):
        result = arrival_features(slots, cutoff)
        result.extend(feature_window(slots, config["specs"], cutoff, metadata_cache))
        return result

    def row(base, slots, target, period):
        result = list(base)
        if config["seasonal"]:
            result.extend(phase_features([float(target)], seasonal_origin, period)[0].tolist())
        result.append(float(target - slots[-1]["until"]) / interval)
        return result

    x, y = [], []
    for anchor in observed:
        cutoff = max(anchor["until"], anchor["receipt"])
        slots = lag_slots(anchor, cutoff)
        if slots is None:
            continue
        period = basis_period
        base = base_row(slots, cutoff)
        # Reproduce the publication delay when choosing historical future
        # intervals. A late interval may not claim to forecast an interval that
        # had already started at the time it became observable.
        offset = config["first"] - ceiling_interval(config["requested"], forecast_interval)
        first = ceiling_interval(cutoff, forecast_interval) + offset
        for step in range(config["steps"]):
            target = targets.get(first + step * forecast_interval)
            if target is not None and target["rate"] is not None:
                x.append(row(base, slots, target["at"], period))
                y.append(target["rate"])
    if len(x) < 8:
        return None, "insufficient causal direct training pairs"
    anchor = observed[-1]
    slots = lag_slots(anchor, config["requested"])
    if slots is None:
        return None, "latest lag window has fewer than half genuine arrival measurements"
    period = basis_period
    base = base_row(slots, config["requested"])
    future = [row(base, slots, config["first"] + step * forecast_interval, period)
              for step in range(config["steps"])]
    cells = (len(x) + len(future)) * len(x[0])
    if cells > MAX_MATRIX_CELLS or cells * config["estimators"] > 200000000:
        raise ValueError("request forecast exceeds the matrix/training budget")
    return (np.asarray(x, dtype=float), np.asarray(y, dtype=float), np.asarray(future, dtype=float),
            latest, period), ""


def input_availability(training, future):
    training_present, future_present = np.isfinite(training), np.isfinite(future)
    return dict(trainingNonMissing=int(training_present.sum()), trainingTotal=int(training_present.size),
                inferenceNonMissing=int(future_present.sum()), inferenceTotal=int(future_present.size),
                trainingRowsWithValue=int(training_present.any(axis=1).sum()),
                inferenceRowsWithValue=int(future_present.any(axis=1).sum()))


def matrix_input_diagnostics(config, training, future):
    lags = config["lags"]
    start = lags + max(1, lags-1) + 2 + 2*lags
    result = {}
    for spec in config["specs"]:
        end = start+3*lags
        result[spec["name"]] = input_availability(training[:, start:end:3], future[:, start:end:3])
        start = end
    return dict(featureAvailability=result,
                arrivalAvailability=input_availability(training[:, :lags], future[:, :lags]))


def xgboost_parameters(config):
    """One effective parameter path for runtime fits and causal replay tests."""
    return dict(n_estimators=config["estimators"], max_depth=config["depth"],
                objective="reg:quantileerror", quantile_alpha=config["alpha"],
                n_jobs=1, verbosity=0, tree_method="hist", random_state=0,
                **config["parameters"])


def predict(payload, regressor_class=None):
    config = parse_request(payload)
    matrices, reason = build_matrices(config)
    result = dict(protocol=PROTOCOL, unit="requests_per_second", generatedAt=config["requested_stamp"],
                  status="warming", reason=reason, forecasts=[])
    if matrices is None:
        return result
    x, y, future, latest, period = matrices
    if regressor_class is None:
        from xgboost import XGBRegressor
        regressor_class = XGBRegressor
    model = regressor_class(**xgboost_parameters(config))
    model.fit(x, y)
    predictions = np.asarray(model.predict(future))
    if predictions.shape != (config["steps"],) or not np.all(np.isfinite(predictions)):
        raise ValueError("request forecast must return one finite value per interval")
    result.update(status="ready", reason="", trainedThrough=stamp(latest["until"]),
                  forecasts=[dict(at=stamp(config["first"] + step * config["forecast_interval"]),
                                  until=stamp(config["first"] + (step + 1) * config["forecast_interval"]),
                                  value=max(0., float(value))) for step, value in enumerate(predictions)],
                  diagnostics=dict(forecastIntervalSeconds=config["forecast_interval"], trainingRows=len(y), featureColumns=x.shape[1],
                                   genuineSamples=sum(item["rate"] is not None for item in config["observations"]),
                                   quantile=config["alpha"], seasonalPeriodSeconds=period,
                                   nEstimators=config["estimators"], maxDepth=config["depth"],
                                   featureNames=[spec["name"] for spec in config["specs"]],
                                   learningRate=config["parameters"]["learning_rate"],
                                   subsample=config["parameters"]["subsample"],
                                   colsampleBytree=config["parameters"]["colsample_bytree"],
                                   minChildWeight=config["parameters"]["min_child_weight"],
                                   gamma=config["parameters"]["gamma"],
                                   regLambda=config["parameters"]["reg_lambda"],
                                   regAlpha=config["parameters"]["reg_alpha"],
                                   objective="reg:quantileerror",
                                   **matrix_input_diagnostics(config, x, future)))
    return result


def main():
    try:
        payload = json.loads(sys.stdin.read(), parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite JSON")))
        if not isinstance(payload, dict):
            raise ValueError("request must be an object")
        sys.stdout.write(json.dumps(predict(payload), allow_nan=False, separators=(",", ":")))
    except (ValueError, KeyError, TypeError) as exc:
        print("invalid request forecast: " + str(exc), file=sys.stderr)
        raise SystemExit(1) from exc


if __name__ == "__main__":
    main()
