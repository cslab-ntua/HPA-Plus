"""Causal CPU/time trend features for clock-based direct regressors.

All inputs are genuine observations ending at the anchor. The features provide
the regressor with local time-normalized motion; they do not replace CPU labels,
forecast values, output clipping, scaler selection or scaling policy.
"""
import math


def trend_lookbacks(window):
    """Distinct bounded contexts, derived from the configured feature window."""
    if type(window) is not int or window < 1:
        raise ValueError("CPU trends require a positive integer configured window")
    if window < 3:
        return ()  # No slope from fewer than three genuine observations.
    return tuple(sorted({min(3, window), min(6, window), window}))


def trend_feature_header(window, projections=True):
    if type(projections) is not bool:
        raise ValueError("trend projections must be boolean")
    lookbacks = trend_lookbacks(window)
    header = ["cpu_ols_slope_%d_samples_m_per_second" % n for n in lookbacks]
    if projections:
        header += ["cpu_ols_projected_%d_samples_m" % n for n in lookbacks]
    return header


def cpu_trend_slopes(cpu_window, time_window):
    """OLS slope per real second, on each bounded observed lookback.

    No interpolation or one-pair derivative is used. The shortest context has
    three genuine observations. Subtracting the final timestamp before centering
    prevents epoch offsets from contaminating the slope calculation.
    """
    import numpy as np
    cpu = np.asarray(cpu_window, dtype=float)
    times = np.asarray(time_window, dtype=float)
    if (cpu.ndim != 1 or times.ndim != 1 or cpu.shape != times.shape
            or not np.all(np.isfinite(cpu)) or np.any(cpu < 0)
            or not np.all(np.isfinite(times))
            or np.any(np.diff(times) <= 0)):
        raise ValueError("CPU trends require finite aligned increasing observations")
    lookbacks = trend_lookbacks(len(cpu))
    slopes = []
    for count in lookbacks:
        clock = times[-count:] - times[-1]
        clock = clock - float(np.mean(clock))
        values = cpu[-count:] - float(np.mean(cpu[-count:]))
        denominator = float(clock @ clock)
        if not math.isfinite(denominator) or denominator <= 0:
            raise ValueError("CPU trend time variance must be positive and finite")
        slope = float(clock @ values) / denominator
        if not math.isfinite(slope):
            raise ValueError("CPU trend slope must be finite")
        slopes.append(slope)
    return np.asarray(slopes, dtype=float)


def cpu_trend_row(slopes, anchor_cpu, elapsed, projections=True):
    """Append unclamped anchor-CPU projections at the exact target elapsed time."""
    import numpy as np
    slopes = np.asarray(slopes, dtype=float)
    if (type(projections) is not bool or slopes.ndim != 1
            or not np.all(np.isfinite(slopes))
            or not math.isfinite(anchor_cpu) or anchor_cpu < 0
            or not math.isfinite(elapsed)
            or elapsed < 0):
        raise ValueError("CPU trend projection requires finite causal inputs")
    values = np.array(slopes, copy=True)
    if projections and len(slopes):
        values = np.concatenate((values, float(anchor_cpu) + slopes * elapsed))
    if not np.all(np.isfinite(values)):
        raise ValueError("CPU trend projection must be finite")
    return values


def cpu_trend_features(cpu_window, time_window, elapsed, projections=True):
    return cpu_trend_row(cpu_trend_slopes(cpu_window, time_window),
                         float(cpu_window[-1]), elapsed, projections)
