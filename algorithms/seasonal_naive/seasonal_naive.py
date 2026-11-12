"""Seasonal naive CPU forecast: each future step repeats the CPU of the same point one cycle earlier.

The cycle length is given in seconds or found from the history itself; without a cycle the
forecast repeats the newest CPU sample. Nothing is fitted. It is the baseline the other
models should beat.
"""
import json
import math
import statistics
import sys
from decimal import Decimal

from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
try:
    from forecast_seasonality import estimate_period
    from forecast_time import forecast_steps, parse_timestamp
    from forecast_trajectory import emit
finally:
    sys.path.pop(0)


def seasonal_naive(times, values, steps, period=None):
    """times are seconds, sorted; returns (forecast values, cycle seconds or None)."""
    if period is None:
        period = estimate_period([float(t - times[0]) for t in times], values)
    interval = max(Decimal("0.001"), statistics.median(b - a for a, b in zip(times, times[1:])))
    last = times[-1]
    out = []
    for step in range(1, steps + 1):
        target = last + interval * step
        if period is None:
            out.append(values[-1])
            continue
        cycle = Decimal(str(period))
        back = target - cycle * math.ceil((target - last) / cycle)
        # Never invent a value: outside the history, or between samples farther than half an
        # interval away, the newest sample stands in.
        nearest = min(range(len(times)), key=lambda i: abs(times[i] - back))
        out.append(values[nearest] if abs(times[nearest] - back) <= interval / 2 else values[-1])
    return out, period


def main():
    payload = json.loads(sys.stdin.read())
    look_ahead = payload["lookAhead"]
    rows = sorted((parse_timestamp(row["time"]), row["time"], row["totalCpuUsageMillicores"])
                  for row in payload["replicaHistory"])
    times, stamps, values = [r[0] for r in rows], [r[1] for r in rows], [float(r[2]) for r in rows]
    period = payload.get("seasonalPeriodSeconds")
    if period is not None and (type(period) not in (int, float) or not math.isfinite(period) or period <= 0):
        raise ValueError("seasonalPeriodSeconds must be a positive number")
    forecast, _ = seasonal_naive(times, values, forecast_steps(look_ahead, times), period)
    if payload.get("returnTrajectory"):
        emit(math.ceil(forecast[-1]), forecast, stamps, look_ahead, math.ceil)
        return
    print(int(math.ceil(forecast[-1])), end="")


if __name__ == "__main__":
    main()
