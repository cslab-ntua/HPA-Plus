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
This linear regression script performs a linear regression using the provided values and configuration using the
statsmodel library.
"""

import sys
import math
from json import JSONDecodeError
from decimal import Decimal
from time import time_ns
from dataclasses import dataclass
from typing import List, Optional
# Resolve shared helpers for both direct script execution and runpy dispatch.
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
try:
    from forecast_trajectory import grid, emit
    from forecast_diagnostics import stage
    from forecast_budget import training_rows
    from forecast_time import parse_timestamp
finally:
    sys.path.pop(0)

import statsmodels.api as sm
from dataclasses_json import dataclass_json, LetterCase

# Takes in a replica history and the look ahead value:
# {
#   "lookAhead": 3,
#   "replicaHistory": [
#       {
#           "time": "2020-02-01T00:55:33Z",
#           "replicas": 3
#       },
#       {
#           "time": "2020-02-01T00:56:33Z",
#           "replicas": 6
#       }
#   ]
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
    look_ahead: int
    replica_history: List[TimestampedReplica]
    return_trajectory: bool = False
    current_time: Optional[str] = None


stdin = sys.stdin.read()

if stdin is None or stdin == "":
    print("No standard input provided to Linear Regression algorithm, exiting", file=sys.stderr)
    sys.exit(1)

try:
    algorithm_input = AlgorithmInput.from_json(stdin)
    training_rows(len(algorithm_input.replica_history))
except JSONDecodeError as ex:
    print(f"Invalid JSON provided: {str(ex)}, exiting", file=sys.stderr)
    sys.exit(1)
except KeyError as ex:
    print(f"Invalid JSON provided: missing {str(ex)}, exiting", file=sys.stderr)
    sys.exit(1)

if algorithm_input.look_ahead <= 0:
    print("lookAhead must be > 0", file=sys.stderr)
    sys.exit(1)

timed_grid = grid([r.time for r in algorithm_input.replica_history], algorithm_input.look_ahead) if algorithm_input.return_trajectory else None

current_time = Decimal(time_ns()) / Decimal(1000000000)

if algorithm_input.current_time is not None:
    try:
        current_time = parse_timestamp(algorithm_input.current_time)
    except ValueError as ex:
        print(f"Invalid datetime format: {str(ex)}", file=sys.stderr)
        sys.exit(1)

search_time = current_time + Decimal(int(algorithm_input.look_ahead)) / Decimal(1000)

x = []
y = []

# Build up data for linear model, in order to not deal with huge values and get rounding errors, use the difference
# between the time being searched for and the metric recorded time in seconds
for i, timestamped_replica in enumerate(algorithm_input.replica_history):
    try:
        created = parse_timestamp(timestamped_replica.time)
    except ValueError as ex:
        print(f"Invalid datetime format: {str(ex)}", file=sys.stderr)
        sys.exit(1)

    x.append(float(search_time - created))
    if timestamped_replica.total_cpu_usage_millicores is None:
        print("Missing total_cpu_usage_millicores in history sample, exiting", file=sys.stderr)
        sys.exit(1)

    y.append(timestamped_replica.total_cpu_usage_millicores)

# Add constant for OLS, constant is 1.0
x = sm.add_constant(x)

with stage("training"):
    model = sm.OLS(y, x).fit()

# Predict the value at the search time (0), include the constant (1).
# The search time is 0 as the values used in training are search time - evaluation time, so the search time will be 0
with stage("inference"):
    predicted = math.ceil(model.predict([[1, 0]])[0])
    if timed_grid is not None:
        end, step, count = timed_grid
        # Evaluate the fitted line at each future timestamp, using the same
        # search-relative feature coordinate as the original scalar forecast.
        values = model.predict([[1, float(search_time-(end+step*(i+1)))] for i in range(count)])
if timed_grid is not None:
    emit(predicted, values, [r.time for r in algorithm_input.replica_history], algorithm_input.look_ahead, math.ceil)
else:
    print(predicted, end="")
