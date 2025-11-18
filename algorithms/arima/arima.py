# Copyright 2022 The Predictive Horizontal Pod Autoscaler Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# pylint: disable=no-member, invalid-name
"""
This ARIMA script performs ARIMA (AutoRegressive Integrated Moving Average) time series forecasting
using the statsmodels library. ARIMA models are capable of capturing complex temporal patterns
including trends and seasonality.
"""

import math
import statistics
import sys
import warnings
from dataclasses import dataclass
from datetime import datetime
from json import JSONDecodeError
from typing import List, Optional, Tuple

import statsmodels.api as sm
from dataclasses_json import LetterCase, dataclass_json
from statsmodels.tools.sm_exceptions import ConvergenceWarning

warnings.simplefilter('ignore', ConvergenceWarning)
warnings.filterwarnings('ignore', message='Non-stationary starting autoregressive parameters found.')
warnings.filterwarnings('ignore', message='Non-invertible starting MA parameters found.')
warnings.filterwarnings('ignore', message='Non-invertible starting seasonal moving average')
warnings.filterwarnings('ignore', message='Non-stationary starting seasonal autoregressive')
warnings.filterwarnings('ignore', message='Too few observations to estimate starting parameters')
warnings.filterwarnings('ignore', message='No frequency information was provided')

# ARIMA configuration input format:
# {
#   "order": [1, 1, 1],
#   "seasonalOrder": [1, 1, 1, 12],
#   "trend": null,
#   "lookAhead": 3,
#   "autoArima": false,
#   "informationCriterion": "aic",
#   "maxOrder": [5, 2, 5],
#   "maxSeasonalOrder": [2, 1, 1, 12],
#   "series": [
#       {
#           "time": "2020-02-01T00:55:33Z",
#           "replicas": 3
#       },
#       {
#           "time": "2020-02-01T00:56:33Z",
#           "replicas": 6
#       }
#   ],
#   "current_time": "2020-02-01T00:57:33Z"
# }


@dataclass_json(letter_case=LetterCase.CAMEL)
@dataclass
class TimestampedReplica:
    """
    JSON data representation of a timestamped evaluation
    """
    time: str
    replicas: int


@dataclass_json(letter_case=LetterCase.CAMEL)
@dataclass
class AlgorithmInput:
    """
    JSON data representation of the data this algorithm requires to be provided to it.
    """
    order: List[int]
    look_ahead: int
    replica_history: List[TimestampedReplica]
    current_time: Optional[str] = None
    seasonal_order: Optional[List[int]] = None
    trend: Optional[str] = None
    auto_arima: bool = False
    information_criterion: str = "aic"
    max_order: Optional[List[int]] = None
    max_seasonal_order: Optional[List[int]] = None
    enforce_stationarity: bool = True
    enforce_invertibility: bool = True
    concentrate_scale: bool = False


def parse_time(time_str: str) -> float:
    """Parse ISO 8601 time string to timestamp"""
    try:
        return datetime.timestamp(datetime.strptime(time_str, "%Y-%m-%dT%H:%M:%SZ"))
    except ValueError as ex:
        print(f"Invalid datetime format: {str(ex)}", file=sys.stderr)
        sys.exit(1)


def validate_arima_input(algorithm_input: AlgorithmInput) -> None:
    """Validate ARIMA input parameters"""
    if not algorithm_input.order or len(algorithm_input.order) != 3:
        print("Invalid ARIMA order provided, must be [p, d, q]", file=sys.stderr)
        sys.exit(1)

    p, d, q = algorithm_input.order
    if p < 0 or d < 0 or q < 0:
        print("ARIMA order parameters must be non-negative", file=sys.stderr)
        sys.exit(1)

    if len(algorithm_input.replica_history) < 3:
        print("Invalid data provided, ARIMA requires at least 3 observations, exiting", file=sys.stderr)
        sys.exit(1)

    if algorithm_input.seasonal_order:
        if len(algorithm_input.seasonal_order) != 4:
            print("Invalid seasonal order provided, must be [P, D, Q, s]", file=sys.stderr)
            sys.exit(1)

        P, D, Q, s = algorithm_input.seasonal_order
        if P < 0 or D < 0 or Q < 0 or s <= 0:
            print("Seasonal order parameters must be non-negative (s must be positive)", file=sys.stderr)
            sys.exit(1)


def sort_history_by_time(replica_history: List[TimestampedReplica]) -> Tuple[List[TimestampedReplica], List[float]]:
    """
    Sort replica history by timestamp. If any timestamps are missing, preserve the original order and
    return an empty timestamp list to signal that time-based calculations should fall back to defaults.
    """
    timestamped_history = []
    for replica in replica_history:
        if replica.time is None:
            return replica_history, []
        timestamped_history.append((parse_time(replica.time), replica))

    timestamped_history.sort(key=lambda entry: entry[0])
    sorted_history = [entry[1] for entry in timestamped_history]
    timestamps = [entry[0] for entry in timestamped_history]

    return sorted_history, timestamps


def calculate_median_interval_ms(timestamps: List[float]) -> int:
    """Return the median interval between timestamps in milliseconds, defaulting to 1000ms when unavailable."""
    if len(timestamps) < 2:
        return 1000

    deltas = [timestamps[i] - timestamps[i - 1] for i in range(1, len(timestamps))]
    positive_deltas = [delta for delta in deltas if delta > 0]

    if not positive_deltas:
        return 1000

    median_seconds = statistics.median(positive_deltas)
    median_ms = int(round(median_seconds * 1000))

    return max(median_ms, 1)


def determine_forecast_steps(look_ahead_ms: int, timestamps: List[float]) -> int:
    """Translate the look ahead (ms) into the number of forecast steps."""
    median_interval_ms = calculate_median_interval_ms(timestamps)
    return max(1, int(round(look_ahead_ms / median_interval_ms)))


def evaluate_configuration(series: List[int], order: Tuple[int, int, int],
                           seasonal: Optional[Tuple[int, int, int, int]],
                           algorithm_input: AlgorithmInput) -> float:
    """Fit a model for a given configuration and return the requested information criterion."""
    model_kwargs = {
        'order': order,
        'trend': algorithm_input.trend,
        'enforce_stationarity': algorithm_input.enforce_stationarity,
        'enforce_invertibility': algorithm_input.enforce_invertibility,
        'concentrate_scale': algorithm_input.concentrate_scale
    }

    if seasonal is not None:
        model_kwargs['seasonal_order'] = seasonal

    model = sm.tsa.ARIMA(series, **model_kwargs)
    fitted = model.fit()

    if algorithm_input.information_criterion == "bic":
        return fitted.bic
    return fitted.aic


def build_seasonal_candidates(algorithm_input: AlgorithmInput) -> List[Optional[Tuple[int, int, int, int]]]:
    """Build seasonal order candidates for auto ARIMA search."""
    candidates: List[Optional[Tuple[int, int, int, int]]] = [None]
    seen = {None}
    manual_seasonal = tuple(algorithm_input.seasonal_order) if algorithm_input.seasonal_order else None

    if len(series) < 6:
        return manual_order, manual_seasonal

    if len(series) < 6:
        return manual_order, manual_seasonal

    if len(series) < 6:
        return manual_order, manual_seasonal

    if manual_seasonal is not None:
        if manual_seasonal not in seen:
            candidates.append(manual_seasonal)
            seen.add(manual_seasonal)

    if algorithm_input.max_seasonal_order:
        max_p, max_d, max_q, max_s = algorithm_input.max_seasonal_order
        if manual_seasonal is not None:
            max_p = max(max_p, manual_seasonal[0])
            max_d = max(max_d, manual_seasonal[1])
            max_q = max(max_q, manual_seasonal[2])
            max_s = max(max_s, manual_seasonal[3])

        if max_s > 0:
            for P in range(max_p + 1):
                for D in range(max_d + 1):
                    for Q in range(max_q + 1):
                        for S in range(1, max_s + 1):
                            seasonal = (P, D, Q, S)
                            if seasonal not in seen:
                                candidates.append(seasonal)
                                seen.add(seasonal)

    return candidates


def auto_select_arima(series: List[int], algorithm_input: AlgorithmInput) -> Tuple[Tuple[int, int, int],
                                                                                  Optional[Tuple[int, int, int, int]]]:
    """Automatically select ARIMA/SARIMA parameters using information criteria."""
    manual_order = tuple(algorithm_input.order)
    max_order = algorithm_input.max_order or [5, 2, 5]
    max_p = max(max_order[0], manual_order[0])
    max_d = manual_order[1]
    max_q = max(max_order[2], manual_order[2])

    manual_seasonal = tuple(algorithm_input.seasonal_order) if algorithm_input.seasonal_order else None
    if len(series) < 6:
        return manual_order, manual_seasonal

    best_ic = float('inf')
    best_config: Optional[Tuple[Tuple[int, int, int], Optional[Tuple[int, int, int, int]]]] = None

    try:
        baseline_ic = evaluate_configuration(series, manual_order, manual_seasonal, algorithm_input)
        best_ic = baseline_ic
        best_config = (manual_order, manual_seasonal)
    except Exception:
        # Baseline configuration invalid, continue searching
        pass

    seasonal_candidates = build_seasonal_candidates(algorithm_input)
    evaluated_configs = set()

    for p in range(max_p + 1):
        for d in range(max_d + 1):
            for q in range(max_q + 1):
                if p == 0 and d == 0 and q == 0:
                    # Skip the completely empty model
                    continue
                for seasonal in seasonal_candidates:
                    config = ((p, d, q), seasonal)
                    if config in evaluated_configs:
                        continue
                    evaluated_configs.add(config)
                    try:
                        ic = evaluate_configuration(series, config[0], config[1], algorithm_input)
                        if ic < best_ic:
                            best_ic = ic
                            best_config = config
                    except Exception:
                        # Skip invalid parameter combinations
                        continue

    if best_config is None:
        raise RuntimeError("auto ARIMA search failed to fit any parameter combinations")

    return best_config


stdin = sys.stdin.read()

if stdin is None or stdin == "":
    print("No standard input provided to ARIMA algorithm, exiting", file=sys.stderr)
    sys.exit(1)

try:
    algorithm_input = AlgorithmInput.from_json(stdin)
except JSONDecodeError as ex:
    print(f"Invalid JSON provided: {str(ex)}, exiting", file=sys.stderr)
    sys.exit(1)
except KeyError as ex:
    print(f"Invalid JSON provided: missing {str(ex)}, exiting", file=sys.stderr)
    sys.exit(1)

# Validate input parameters
validate_arima_input(algorithm_input)

replica_history, timestamps = sort_history_by_time(algorithm_input.replica_history)

# Extract time series data from replica history
series = [replica.replicas for replica in replica_history]

try:
    seasonal_order = tuple(algorithm_input.seasonal_order) if algorithm_input.seasonal_order else None

    # Auto-select ARIMA parameters if requested
    if algorithm_input.auto_arima:
        selected_order, selected_seasonal = auto_select_arima(series, algorithm_input)
        seasonal_order = selected_seasonal
        seasonal_msg = selected_seasonal if selected_seasonal is not None else "none"
        print(f"# Auto-selected ARIMA order: {selected_order}, seasonal_order: {seasonal_msg}", file=sys.stderr)
        arima_order = selected_order
    else:
        arima_order = tuple(algorithm_input.order)

    # Create and fit ARIMA model
    model_kwargs = {
        'order': arima_order,
        'trend': algorithm_input.trend,
        'enforce_stationarity': algorithm_input.enforce_stationarity,
        'enforce_invertibility': algorithm_input.enforce_invertibility,
        'concentrate_scale': algorithm_input.concentrate_scale
    }

    # Add seasonal parameters if provided
    if seasonal_order is not None:
        if len(series) < seasonal_order[3]:
            raise ValueError(f"seasonal order requires at least {seasonal_order[3]} observations")
        model_kwargs['seasonal_order'] = seasonal_order

    model = sm.tsa.ARIMA(series, **model_kwargs)
    fitted_model = model.fit()

    # Forecast ahead for the specified number of steps (based on timestamps when available)
    forecast_steps = determine_forecast_steps(algorithm_input.look_ahead, timestamps)
    forecast = fitted_model.forecast(steps=forecast_steps)

    # Return the forecast value at the requested horizon, rounded up to nearest integer
    if len(forecast) > 0:
        prediction = math.ceil(forecast[-1])
    else:
        prediction = series[-1]

    if len(series) >= 4:
        diffs = [series[i] - series[i - 1] for i in range(len(series) - 3, len(series))]
        if all(diff > 0 for diff in diffs):
            recent_trend = diffs[-1]
            min_growth = series[-1] + recent_trend * forecast_steps
            if prediction < min_growth:
                prediction = min_growth

    print(prediction, end="")

except Exception as ex:
    print(f"ARIMA model fitting failed: {str(ex)}, falling back to last observed value", file=sys.stderr)
    # Fallback to the last observed value if ARIMA fails
    fallback_value = series[-1] if series else 1
    print(fallback_value, end="")
