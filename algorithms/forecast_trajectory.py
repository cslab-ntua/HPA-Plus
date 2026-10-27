"""Shared bounded intervals for model-native forecasts; no interpolation."""
import json
import math
import statistics
from decimal import Decimal

from forecast_diagnostics import format_timestamp
from forecast_time import parse_timestamp, forecast_steps

MAX_POINTS = 32


def grid(timestamps, look_ahead=None):
    if any(not isinstance(t, (str, Decimal)) for t in timestamps):
        raise ValueError("timed forecasts require valid timestamps")
    stamps = sorted(parse_timestamp(t) if isinstance(t, str) else t for t in timestamps)
    if len(stamps) < 2 or any(b <= a for a, b in zip(stamps, stamps[1:])):
        raise ValueError("timed forecasts require distinct timestamped history")
    step = max(Decimal("0.001"), statistics.median(b-a for a, b in zip(stamps, stamps[1:])))
    count = 1 if look_ahead is None else forecast_steps(look_ahead, stamps)
    if count > MAX_POINTS:
        raise ValueError("timed forecast exceeds the 32-point budget")
    if step > 86400:
        raise ValueError("timed forecast cadence exceeds one day")
    return stamps[-1], step, count


def result(prediction, values, timestamps, look_ahead=None, rounding=round):
    end, step, count = grid(timestamps, look_ahead)
    if len(values) != count:
        raise ValueError("trajectory does not cover the forecast horizon")
    points = []
    for i, value in enumerate(values):
        value = float(value)
        if not math.isfinite(value) or not -(2**63) <= value < 2**63:
            raise ValueError("forecast must contain finite int64 CPU millicores")
        cpu = max(0, int(rounding(value)))
        if cpu >= 2**63:
            raise ValueError("rounded forecast exceeds int64")
        points.append({"at": format_timestamp(end + step*(i+1)),
                       "until": format_timestamp(end + step*(i+2)), "cpu": cpu})
    return {"prediction": int(prediction), "trajectory": points}


def emit(prediction, values, timestamps, look_ahead=None, rounding=round):
    print(json.dumps(result(prediction, values, timestamps, look_ahead, rounding),
                     allow_nan=False, separators=(",", ":")), end="")
