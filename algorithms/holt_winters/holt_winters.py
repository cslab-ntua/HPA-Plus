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
This holt winters script performs a holt winters calculation using the provided values and configuration using the
statsmodel library.
"""

import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
try:
    from forecast_trajectory import grid, emit
    from forecast_diagnostics import stage
    from forecast_budget import training_rows
    from forecast_time import holt_forecast_steps
finally:
    sys.path.pop(0)
import math
from json import JSONDecodeError
from dataclasses import dataclass
from typing import List, Optional
import warnings
import statsmodels.tsa.api as sm
from dataclasses_json import dataclass_json, LetterCase
from statsmodels.tools.sm_exceptions import ConvergenceWarning

warnings.simplefilter('ignore', ConvergenceWarning)

# Takes in the replica series, alpha, beta, gamma, season length, and the trend and seasonal (add vs mul)
# {
#   "trend": "add",
#   "seasonal": "mul"
#   "alpha": 0.1,
#   "beta": 0.1,
#   "gamma": 0.1,
#   "seasonalPeriods": 5,
#   "series": [
#       0,
#       1,
#       2,
#       3,
#       4
#   ],
#   "dampedTrend": false,
# }


@dataclass_json(letter_case=LetterCase.CAMEL)
@dataclass
class AlgorithmInput:
    """
    JSON data representation of the data this algorithm requires to be provided to it.
    """
    trend: str
    seasonal: str
    seasonal_periods: int
    alpha: float
    beta: float
    gamma: float
    series: List[int]
    return_trajectory: bool = False
    look_ahead: Optional[int] = None
    timestamps: Optional[List[str]] = None
    damped_trend: bool = False
    initialization_method: str = "estimated"
    initial_level: Optional[float] = None
    initial_trend: Optional[float] = None
    initial_seasonal: Optional[float] = None


stdin = sys.stdin.read()

if stdin is None or stdin == "":
    print("No standard input provided to Holt-Winters algorithm, exiting", file=sys.stderr)
    sys.exit(1)

try:
    algorithm_input = AlgorithmInput.from_json(stdin)
except JSONDecodeError as ex:
    print(f"Invalid JSON provided: {str(ex)}, exiting", file=sys.stderr)
    sys.exit(1)
except KeyError as ex:
    print(f"Invalid JSON provided: missing {str(ex)}, exiting", file=sys.stderr)
    sys.exit(1)

if algorithm_input.seasonal_periods < 2:
    print("seasonalPeriods must be >= 2", file=sys.stderr)
    sys.exit(1)
if any(value is None or not math.isfinite(value) or not 0 <= value <= 1
       for value in (algorithm_input.alpha, algorithm_input.beta, algorithm_input.gamma)):
    print("Smoothing factors must be finite and in [0, 1]", file=sys.stderr)
    sys.exit(1)
initial_values = (algorithm_input.initial_level, algorithm_input.initial_trend, algorithm_input.initial_seasonal)
if algorithm_input.initialization_method == "known":
    if any(value is None or not math.isfinite(value) for value in initial_values):
        print("Known initialization requires finite level, trend, and seasonal values", file=sys.stderr)
        sys.exit(1)
elif any(value is not None for value in initial_values):
    print("Initial values require initializationMethod known", file=sys.stderr)
    sys.exit(1)
if algorithm_input.initialization_method not in ("known", "estimated", "heuristic", "legacy-heuristic"):
    print("Invalid initializationMethod", file=sys.stderr)
    sys.exit(1)
if any(not math.isfinite(value) or value < 0 for value in algorithm_input.series):
    print("CPU history must be finite and non-negative", file=sys.stderr)
    sys.exit(1)
if (algorithm_input.trend in ("mul", "multiplicative") or algorithm_input.seasonal in ("mul", "multiplicative")) and any(value <= 0 for value in algorithm_input.series):
    print("Multiplicative Holt-Winters requires strictly positive CPU history", file=sys.stderr)
    sys.exit(1)

if len(algorithm_input.series) < 2 * algorithm_input.seasonal_periods:
    print("Invalid data provided, must be at least 2 * seasonal_periods observations, exiting", file=sys.stderr)
    sys.exit(1)

if len(algorithm_input.series) < 10 + 2 * (algorithm_input.seasonal_periods // 2):
    print("Invalid data provided, must be at least 10 + 2 * (seasonal_periods // 2) observations, exiting",
          file=sys.stderr)
    sys.exit(1)

training_rows(len(algorithm_input.series))
steps = holt_forecast_steps(algorithm_input.look_ahead, algorithm_input.timestamps, len(algorithm_input.series))
if algorithm_input.return_trajectory:
    grid(algorithm_input.timestamps or [], algorithm_input.look_ahead)
model = sm.ExponentialSmoothing(algorithm_input.series,
                                trend=algorithm_input.trend,
                                seasonal=algorithm_input.seasonal,
                                seasonal_periods=algorithm_input.seasonal_periods,
                                initialization_method=algorithm_input.initialization_method,
                                damped_trend=algorithm_input.damped_trend,
                                initial_level=algorithm_input.initial_level,
                                initial_trend=algorithm_input.initial_trend,
                                initial_seasonal=algorithm_input.initial_seasonal)

with stage("training"):
    fitted_model = model.fit(smoothing_level=algorithm_input.alpha,
                             smoothing_trend=algorithm_input.beta,
                             smoothing_seasonal=algorithm_input.gamma,
                             # The API exposes damping as a switch, not a fixed phi.
                             # statsmodels needs optimization to estimate that absent
                             # coefficient. Supplied alpha/beta/gamma remain fixed.
                             optimized=algorithm_input.damped_trend)

# Return the first discrete point at or beyond the requested horizon.
with stage("inference"):
    values = fitted_model.forecast(steps=steps)
    predicted = math.ceil(values[-1])
if algorithm_input.return_trajectory:
    emit(predicted, values, algorithm_input.timestamps, algorithm_input.look_ahead, math.ceil)
else:
    print(predicted, end="")
