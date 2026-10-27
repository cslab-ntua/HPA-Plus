"""Shared UTC timestamp and discrete forecast horizon semantics.

Exact decimal epoch seconds retain RFC3339Nano ordering without floating-point
collapse. Conversion to floating point is deferred until numerical model inputs.
Missing timestamps retain the existing caller-specific compatibility policy.
"""
from datetime import datetime, timezone
from decimal import Decimal, ROUND_CEILING
import re
import statistics

_PATTERN = re.compile(r"([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\.([0-9]{1,9}))?(Z|[+-][0-9]{2}:[0-9]{2})")
_EPOCH = datetime(1970, 1, 1, tzinfo=timezone.utc)
# Bound recursive tree work and statsmodels forecast allocations independently
# of process deadlines. This is a step count, not a duration or training limit.
MAX_FORECAST_STEPS = 10000


def parse_timestamp(value):
    """Parse a timezone-qualified RFC3339 instant, retaining up to nine digits."""
    match = _PATTERN.fullmatch(value) if isinstance(value, str) else None
    if match is None:
        raise ValueError("timestamps require RFC3339 with an explicit timezone and at most nine fractional digits")
    base, fraction, offset = match.groups()
    # Python 3.14 accepts ISO 8601 24:00 as next-day midnight. Our history
    # protocol is RFC3339 and must reject normalization of invalid clock fields.
    if int(base[11:13]) > 23 or int(base[14:16]) > 59 or int(base[17:19]) > 59:
        raise ValueError("invalid RFC3339 clock time")
    if offset != "Z" and (int(offset[1:3]) > 23 or int(offset[4:6]) > 59):
        raise ValueError("invalid RFC3339 timezone offset")
    instant = datetime.fromisoformat(base + ("+00:00" if offset == "Z" else offset))
    delta = instant - _EPOCH
    seconds = Decimal(delta.days * 86400 + delta.seconds)
    return seconds + Decimal(int((fraction or "").ljust(9, "0"))) / Decimal(1000000000)


def median_interval_ms(timestamps):
    """Median positive spacing rounded to milliseconds, minimum 1ms, fallback 1s.

    Callers supply chronological timestamps. Irregular-spacing rejection is a
    separate model policy; this retains existing interval estimation behavior.
    """
    deltas = [right-left for left, right in zip(timestamps, timestamps[1:]) if right > left]
    if not deltas:
        return 1000
    return max(1, int(round(statistics.median(deltas) * 1000)))


def forecast_steps(look_ahead_ms, timestamps):
    """First estimated step at/beyond the horizon, within the runtime budget."""
    if type(look_ahead_ms) is not int or look_ahead_ms <= 0:
        raise ValueError("lookAhead must be a positive integer in milliseconds")
    # A virtual uniform grid at the exact median positive interval. Do not
    # interpolate values or round the interval up: that could undershoot time.
    deltas = [right-left for left, right in zip(timestamps, timestamps[1:]) if right > left]
    interval = max(Decimal("0.001"), statistics.median(deltas)) * 1000 if deltas else Decimal(1000)
    steps = int((Decimal(look_ahead_ms) / interval).to_integral_value(rounding=ROUND_CEILING))
    if steps > MAX_FORECAST_STEPS:
        raise ValueError("forecast horizon exceeds the 10000-step runtime budget")
    return steps


def holt_forecast_steps(look_ahead_ms, timestamps, count):
    """Legacy series-only callers get one step; explicit horizons need timestamps."""
    if timestamps is None:
        if look_ahead_ms is not None:
            raise ValueError("Holt-Winters lookAhead requires timestamps")
        return 1
    if not isinstance(timestamps, list) or len(timestamps) != count:
        raise ValueError("Holt-Winters timestamps must match series")
    parsed = [parse_timestamp(value) for value in timestamps]
    if any(right <= left for left, right in zip(parsed, parsed[1:])):
        raise ValueError("Holt-Winters timestamps must be strictly increasing")
    return 1 if look_ahead_ms is None else forecast_steps(look_ahead_ms, parsed)
