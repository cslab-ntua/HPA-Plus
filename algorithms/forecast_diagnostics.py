"""Opt-in, bounded forecast provenance and independent monotonic stage timings."""
from contextlib import contextmanager
from contextvars import ContextVar
from datetime import datetime, timedelta, timezone
from decimal import Decimal
from importlib.metadata import version
from time import perf_counter_ns
import statistics

from forecast_time import parse_timestamp, forecast_steps

_active = ContextVar("forecast_diagnostics", default=None)
ENGINES = {"Linear": "statsmodels", "HoltWinters": "statsmodels", "ARIMA": "statsmodels",
           "XGBoost": "xgboost", "LightGBM": "lightgbm"}
MAX_STAGE_NS = 86400 * 1000000000


@contextmanager
def capture():
    timings = {"trainingNanoseconds": 0, "inferenceNanoseconds": 0}
    token = _active.set(timings)
    try:
        yield timings
    finally:
        _active.reset(token)


@contextmanager
def stage(name):
    timings = _active.get()
    started = perf_counter_ns()
    try:
        yield
    finally:
        if timings is not None:
            key = name + "Nanoseconds"
            timings[key] = min(MAX_STAGE_NS, timings[key] + max(0, perf_counter_ns() - started))


def format_timestamp(value):
    nanos = int(value * Decimal(1000000000))
    seconds, fraction = divmod(nanos, 1000000000)
    instant = datetime(1970, 1, 1, tzinfo=timezone.utc) + timedelta(seconds=seconds)
    return f"{instant.year:04d}-{instant.month:02d}-{instant.day:02d}T{instant.hour:02d}:{instant.minute:02d}:{instant.second:02d}" + (".%09d" % fraction if fraction else "") + "Z"


def describe(model, payload, timings, action="one_shot", timestamps=None):
    if timestamps is None:
        raw = payload.get("timestamps") if model == "HoltWinters" else [
            row["time"] for row in payload["replicaHistory"]]
        timestamps = sorted(parse_timestamp(value) for value in raw) if raw is not None else []
    count = len(payload.get("series", [])) if model == "HoltWinters" else len(timestamps)
    look_ahead = payload.get("lookAhead")
    steps = 1 if (model == "Linear" and not payload.get("returnTrajectory")) or look_ahead is None else forecast_steps(look_ahead, timestamps)
    engine = ENGINES[model]
    result = {"schemaVersion": 1, "model": model, "engine": engine, "engineVersion": version("xgboost-cpu" if model == "XGBoost" else engine),
              "action": action, "windowSamples": count, "forecastSteps": steps,
              "uncertainty": "unavailable", **timings}
    if timestamps:
        deltas = [right-left for left, right in zip(timestamps, timestamps[1:])]
        interval = max(Decimal("0.001"), statistics.median(deltas)) if deltas else Decimal(1)
        horizon = Decimal(look_ahead) / 1000 if model == "Linear" and not payload.get("returnTrajectory") else interval * steps
        result.update(historyStart=format_timestamp(timestamps[0]), historyEnd=format_timestamp(timestamps[-1]),
                      forecastAt=format_timestamp(timestamps[-1] + horizon))
    return result
