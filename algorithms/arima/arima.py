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
This ARIMA script performs non-seasonal ARIMA (AutoRegressive Integrated Moving Average) time series
forecasting using the statsmodels library. ARIMA models capture complex temporal patterns including
overall trends.
"""

import math
from decimal import Decimal
import sys
import warnings
from dataclasses import dataclass
from json import JSONDecodeError
from typing import List, Optional, Tuple

# Resolve shared helpers for both direct script execution and runpy dispatch.
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
try:
    from forecast_trajectory import grid, emit
    from forecast_diagnostics import stage
    from forecast_budget import arima_budget
    from forecast_time import parse_timestamp, median_interval_ms as shared_interval_ms, forecast_steps as shared_forecast_steps
finally:
    sys.path.pop(0)

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
#   "trend": null,
#   "lookAhead": 3,
#   "autoArima": false,
#   "informationCriterion": "aic",
#   "maxOrder": [5, 2, 5],
#   "series": [
#       {
#           "time": "2020-02-01T00:55:33Z",
#           "replicas": 3,
#           "totalCpuUsageMillicores": 1800
#       },
#       {
#           "time": "2020-02-01T00:56:33Z",
#           "replicas": 6,
#           "totalCpuUsageMillicores": 2100
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
    total_cpu_usage_millicores: Optional[int] = None


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
    return_trajectory: bool = False
    trend: Optional[str] = None
    auto_arima: bool = False
    information_criterion: str = "aic"
    max_order: Optional[List[int]] = None
    enforce_stationarity: bool = True
    enforce_invertibility: bool = True
    concentrate_scale: bool = False
    use_sarima: bool = False
    seasonal_order: Optional[List[int]] = None
    seasonal_periods: Optional[int] = None


def parse_time(time_str):
    try:
        return parse_timestamp(time_str)
    except ValueError as exc:
        print(f"Invalid datetime format: {exc}", file=sys.stderr)
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

    if algorithm_input.look_ahead <= 0:
        raise ValueError("lookAhead must be > 0")
    if algorithm_input.trend not in (None, "n", "c", "t", "ct"):
        raise ValueError("invalid ARIMA trend")
    if algorithm_input.information_criterion not in ("aic", "bic"):
        raise ValueError("informationCriterion must be aic or bic")
    if algorithm_input.max_order is not None:
        if len(algorithm_input.max_order) != 3 or any(value < 0 for value in algorithm_input.max_order):
            raise ValueError("maxOrder must contain three non-negative integers")
        if algorithm_input.auto_arima and any(value > bound for value, bound in zip(algorithm_input.order, algorithm_input.max_order)):
            raise ValueError("order must not exceed maxOrder during auto selection")

    if algorithm_input.use_sarima or algorithm_input.seasonal_order is not None or algorithm_input.seasonal_periods is not None:
        if algorithm_input.auto_arima:
            raise ValueError("autoArima supports non-seasonal ARIMA only")
        if algorithm_input.seasonal_order is None or len(algorithm_input.seasonal_order) != 3:
            print("Invalid SARIMA seasonal_order provided, must be [P, D, Q]", file=sys.stderr)
            sys.exit(1)
        if algorithm_input.seasonal_periods is None or algorithm_input.seasonal_periods < 2:
            print("Invalid SARIMA seasonal_periods provided, must be >= 2", file=sys.stderr)
            sys.exit(1)
        P, D, Q = algorithm_input.seasonal_order
        if P < 0 or D < 0 or Q < 0:
            print("SARIMA seasonal order parameters must be non-negative", file=sys.stderr)
            sys.exit(1)
    elif not algorithm_input.auto_arima and ((d > 0 and algorithm_input.trend in ("c", "ct")) or (d > 1 and algorithm_input.trend == "t")):
        raise ValueError("ARIMA trend degree must be at least the differencing order")


def sort_history_by_time(replica_history: List[TimestampedReplica]) -> Tuple[List[TimestampedReplica], List[Decimal]]:
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


def calculate_median_interval_ms(timestamps):
    return shared_interval_ms(timestamps)


def extract_cpu_usage_series(replica_history: List[TimestampedReplica]) -> List[float]:
    """Extract aggregate CPU usage series from history."""
    series = []
    for replica in replica_history:
        if replica.total_cpu_usage_millicores is None:
            print("Missing totalCpuUsageMillicores in replica history, exiting", file=sys.stderr)
            sys.exit(1)
        series.append(float(replica.total_cpu_usage_millicores))
    return series


def determine_forecast_steps(look_ahead_ms, timestamps):
    return shared_forecast_steps(look_ahead_ms, timestamps)


def evaluate_configuration(series: List[int], order: Tuple[int, int, int],
                           algorithm_input: AlgorithmInput) -> float:
    """Fit a model for a given configuration and return the requested information criterion."""
    arima_budget(len(series), order, algorithm_input.seasonal_order, algorithm_input.seasonal_periods)
    model_kwargs = {
        'order': order,
        'trend': algorithm_input.trend,
        'enforce_stationarity': algorithm_input.enforce_stationarity,
        'enforce_invertibility': algorithm_input.enforce_invertibility,
        'concentrate_scale': algorithm_input.concentrate_scale
    }

    model = sm.tsa.ARIMA(series, **model_kwargs)
    fitted = model.fit()

    if algorithm_input.information_criterion == "bic":
        return fitted.bic
    return fitted.aic


def auto_select_arima(series: List[int], algorithm_input: AlgorithmInput) -> Tuple[int, int, int]:
    """Automatically select non-seasonal ARIMA parameters using information criteria."""
    manual_order = tuple(algorithm_input.order)
    max_order = algorithm_input.max_order or [max(5, manual_order[0]), max(2, manual_order[1]), max(5, manual_order[2])]
    arima_budget(len(series), manual_order, max_order=max_order)
    max_p, max_d, max_q = max_order

    if len(series) < 6:
        return manual_order

    best_ic = float('inf')
    best_config: Optional[Tuple[int, int, int]] = None

    try:
        baseline_ic = evaluate_configuration(series, manual_order, algorithm_input)
        if math.isfinite(baseline_ic):
            best_ic = baseline_ic
            best_config = manual_order
    except Exception:
        # Baseline configuration invalid, continue searching
        pass

    evaluated_configs = set()

    for p in range(max_p + 1):
        for d in range(max_d + 1):
            for q in range(max_q + 1):
                if p == 0 and d == 0 and q == 0:
                    # Skip the completely empty model
                    continue
                config = (p, d, q)
                if config in evaluated_configs:
                    continue
                evaluated_configs.add(config)
                try:
                    ic = evaluate_configuration(series, config, algorithm_input)
                    if math.isfinite(ic) and ic < best_ic:
                        best_ic = ic
                        best_config = config
                except Exception:
                    # Skip invalid parameter combinations
                    continue

    if best_config is None:
        raise RuntimeError("auto ARIMA search failed to fit any parameter combinations")

    return best_config


def build_model(series: List[float], algorithm_input: AlgorithmInput,
                arima_order: Tuple[int, int, int],
                exog: Optional[List[List[float]]] = None):
    """Build an ARIMA/SARIMA model, switching to SARIMAX when exogenous regressors are used."""
    arima_budget(len(series), arima_order, algorithm_input.seasonal_order, algorithm_input.seasonal_periods)
    use_sarima = algorithm_input.use_sarima or algorithm_input.seasonal_order is not None or algorithm_input.seasonal_periods is not None

    if use_sarima or exog is not None:
        seasonal_order = tuple(algorithm_input.seasonal_order or [0, 0, 0])
        seasonal_periods = algorithm_input.seasonal_periods or 0
        return sm.tsa.SARIMAX(
            series,
            exog=exog,
            order=arima_order,
            seasonal_order=seasonal_order + (seasonal_periods,),
            trend=algorithm_input.trend,
            enforce_stationarity=algorithm_input.enforce_stationarity,
            enforce_invertibility=algorithm_input.enforce_invertibility,
            concentrate_scale=algorithm_input.concentrate_scale
        )

    model_kwargs = {
        'order': arima_order,
        'trend': algorithm_input.trend,
        'enforce_stationarity': algorithm_input.enforce_stationarity,
        'enforce_invertibility': algorithm_input.enforce_invertibility,
        'concentrate_scale': algorithm_input.concentrate_scale
    }
    return sm.tsa.ARIMA(series, **model_kwargs)


def fit_model(model):
    """Fit a statsmodels ARIMA-family model while handling older signatures."""
    try:
        return model.fit(disp=False)
    except TypeError:
        return model.fit()


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

# Extract time series data from aggregate CPU usage history
series = extract_cpu_usage_series(replica_history)

try:
    # Reject oversized forecast allocations before auto-search or fitting.
    forecast_steps = determine_forecast_steps(algorithm_input.look_ahead, timestamps)
    if algorithm_input.return_trajectory:
        grid(timestamps, algorithm_input.look_ahead)
    with stage("training"):
        # Auto-select ARIMA parameters if requested
        if algorithm_input.auto_arima:
            selected_order = auto_select_arima(series, algorithm_input)
            print(f"# Auto-selected ARIMA order: {selected_order}", file=sys.stderr)
            arima_order = selected_order
        else:
            arima_order = tuple(algorithm_input.order)

        # Forecast ahead for the specified number of steps (based on timestamps when available)
        model = build_model(series, algorithm_input, arima_order)
        fitted_model = fit_model(model)
    with stage("inference"):
        forecast = fitted_model.forecast(steps=forecast_steps)

    # Return the forecast value at the requested horizon, rounded up to nearest integer
    if len(forecast) == 0:
        raise ValueError("ARIMA returned an empty forecast")
    prediction = math.ceil(forecast[-1])

    if algorithm_input.return_trajectory:
        emit(prediction, forecast, timestamps, algorithm_input.look_ahead, math.ceil)
    else:
        print(int(prediction), end="")

except Exception as ex:
    print(f"ARIMA forecasting failed: {str(ex)}", file=sys.stderr)
    # The controller owns fallback and backoff. A copied observation must not
    # be persisted as a successfully fitted forecast or advance model cadence.
    sys.exit(1)
