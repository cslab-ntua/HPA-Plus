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

import sys
import math
import warnings
from json import JSONDecodeError
from dataclasses import dataclass
from typing import List, Optional, Tuple
import statsmodels.api as sm
from dataclasses_json import dataclass_json, LetterCase
from statsmodels.tools.sm_exceptions import ConvergenceWarning

warnings.simplefilter('ignore', ConvergenceWarning)

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
        from datetime import datetime
        return datetime.timestamp(datetime.strptime(time_str, "%Y-%m-%dT%H:%M:%SZ"))
    except ValueError as ex:
        print(f"Invalid datetime format: {str(ex)}", file=sys.stderr)
        sys.exit(1)


def validate_arima_input(algorithm_input: AlgorithmInput) -> None:
    """Validate ARIMA input parameters"""
    if len(algorithm_input.replica_history) < 3:
        print("Invalid data provided, ARIMA requires at least 3 observations, exiting", file=sys.stderr)
        sys.exit(1)

    if not algorithm_input.order or len(algorithm_input.order) != 3:
        print("Invalid ARIMA order provided, must be [p, d, q]", file=sys.stderr)
        sys.exit(1)

    p, d, q = algorithm_input.order
    if p < 0 or d < 0 or q < 0:
        print("ARIMA order parameters must be non-negative", file=sys.stderr)
        sys.exit(1)

    if algorithm_input.seasonal_order:
        if len(algorithm_input.seasonal_order) != 4:
            print("Invalid seasonal order provided, must be [P, D, Q, s]", file=sys.stderr)
            sys.exit(1)

        P, D, Q, s = algorithm_input.seasonal_order
        if P < 0 or D < 0 or Q < 0 or s <= 0:
            print("Seasonal order parameters must be non-negative (s must be positive)", file=sys.stderr)


def auto_select_arima(series: List[int], algorithm_input: AlgorithmInput) -> Tuple:
    """Automatically select ARIMA parameters using information criteria"""
    max_p = algorithm_input.max_order[0] if algorithm_input.max_order else 5
    max_d = algorithm_input.max_order[1] if algorithm_input.max_order else 2
    max_q = algorithm_input.max_order[2] if algorithm_input.max_order else 5

    best_ic = float('inf')
    best_order = (1, 1, 1)

    # Grid search for best ARIMA parameters
    for p in range(max_p + 1):
        for d in range(max_d + 1):
            for q in range(max_q + 1):
                try:
                    if p == 0 and d == 0 and q == 0:
                        continue

                    model = sm.tsa.ARIMA(series, order=(p, d, q))
                    fitted = model.fit()

                    if algorithm_input.information_criterion == "aic":
                        ic = fitted.aic
                    elif algorithm_input.information_criterion == "bic":
                        ic = fitted.bic
                    else:
                        ic = fitted.aic

                    if ic < best_ic:
                        best_ic = ic
                        best_order = (p, d, q)

                except Exception:
                    # Skip invalid parameter combinations
                    continue

    return best_order


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

# Extract time series data from replica history
series = [replica.replicas for replica in algorithm_input.replica_history]

try:
    # Auto-select ARIMA parameters if requested
    if algorithm_input.auto_arima:
        selected_order = auto_select_arima(series, algorithm_input)
        print(f"# Auto-selected ARIMA order: {selected_order}", file=sys.stderr)
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
    if algorithm_input.seasonal_order:
        model_kwargs['seasonal_order'] = tuple(algorithm_input.seasonal_order)

    model = sm.tsa.ARIMA(series, **model_kwargs)
    fitted_model = model.fit()

    # Forecast ahead for the specified number of steps
    forecast_steps = max(1, algorithm_input.look_ahead // 1000)  # Convert ms to steps, min 1
    forecast = fitted_model.forecast(steps=forecast_steps)

    # Return the first forecast value, rounded up to nearest integer
    prediction = math.ceil(forecast[0]) if len(forecast) > 0 else series[-1]

    print(prediction, end="")

except Exception as ex:
    print(f"ARIMA model fitting failed: {str(ex)}, falling back to last observed value", file=sys.stderr)
    # Fallback to the last observed value if ARIMA fails
    fallback_value = series[-1] if series else 1
    print(fallback_value, end="")