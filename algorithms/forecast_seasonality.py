"""Optional cycle features inferred from irregularly timed past CPU samples."""
import math
import numpy as np


def estimate_period(times, values):
    """Find a supported clock period without interpolating missing CPU.

    A bounded three-harmonic fit avoids favoring long repetitions merely because
    their lag-matched subsets contain quieter samples. A joint linear trend
    keeps nonperiodic drift out of the cycle score. The variance gate and local
    peak checks are heuristics, not a calibrated confidence guarantee.
    """
    times, values = np.asarray(times, dtype=float), np.asarray(values, dtype=float)
    if (len(times) < 24 or len(times) != len(values)
            or not np.all(np.isfinite(times)) or not np.all(np.isfinite(values))
            or np.any(np.diff(times) <= 0)):
        return None
    times = times-times[0]
    cadence, span = float(np.median(np.diff(times))), float(times[-1])
    if span < 12*cadence or np.var(values) < 1:
        return None
    trend = np.column_stack((np.ones(len(times)), times/span))
    residual = values-trend@np.linalg.lstsq(trend, values, rcond=None)[0]
    total = float(residual@residual)
    if total < 1e-6:
        return None

    def fit(period):
        phase = 2*np.pi*times/period
        features = np.column_stack([np.ones(len(times)), times/span]
            + [function(harmonic*phase) for harmonic in (1, 2, 3)
               for function in (np.sin, np.cos)])
        delta = values-features@np.linalg.lstsq(features, values, rcond=None)[0]
        return 1-float(delta@delta)/total

    # Six nominal samples per cycle resolve the three harmonics. At most 223
    # coarse trials and eleven refinement trials; each matrix has eight columns.
    scores = {float(period): fit(period) for period in
              np.arange(max(90., 6*cadence), min(1200., span/2)+1, 5.)}
    if not scores:
        return None
    best = max(scores, key=scores.get)
    threshold = max(.4, 1-np.exp(-(6*np.log(len(times))+2*np.log(len(scores)))/len(times)))
    if scores[best] < threshold:
        return None
    distance = max(cadence, .08*best)
    left = [period for period in scores if period <= best-distance]
    right = [period for period in scores if period >= best+distance]
    if (not left or not right
            or scores[best] < max(scores[max(left)], scores[min(right)])+.02):
        return None
    fine = {period: fit(period) for period in
            np.arange(max(min(scores), best-5), min(max(scores), best+5)+.01, 1.)}
    return float(max(fine, key=fine.get))


def phase_features(times, origin, period):
    if period is None:
        return np.full((len(times),3), np.nan)
    phase = ((np.asarray(times,dtype=float)-origin)/period) % 1
    return np.column_stack((phase, np.sin(2*math.pi*phase), np.cos(2*math.pi*phase)))
