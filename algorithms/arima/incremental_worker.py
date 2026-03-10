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
Stateful ARIMA/SARIMA worker that supports:

- fit_forecast: fit from full history and forecast.
- append_forecast: update fitted state with new observations and forecast.
- forecast: forecast from existing fitted state.
- reset: clear state.

Requests and responses are newline-delimited JSON over stdin/stdout.
"""

import json
import math
import statistics
import sys
import warnings
from datetime import datetime
from typing import Dict, List, Optional, Tuple

import statsmodels.api as sm
from statsmodels.tools.sm_exceptions import ConvergenceWarning

warnings.simplefilter("ignore", ConvergenceWarning)
warnings.filterwarnings("ignore", message="Non-stationary starting autoregressive parameters found.")
warnings.filterwarnings("ignore", message="Non-invertible starting MA parameters found.")
warnings.filterwarnings("ignore", message="Non-invertible starting seasonal moving average")
warnings.filterwarnings("ignore", message="Non-stationary starting seasonal autoregressive")
warnings.filterwarnings("ignore", message="Too few observations to estimate starting parameters")
warnings.filterwarnings("ignore", message="No frequency information was provided")


_state_result = None
_state_series: List[float] = []
_state_timestamps: List[float] = []
_state_config: Optional[Dict] = None


def parse_time(time_str: Optional[str]) -> Optional[float]:
    if time_str is None:
        return None
    try:
        # Kubernetes serializes timestamps as RFC3339 / RFC3339Nano.
        normalized = time_str
        if normalized.endswith("Z"):
            normalized = normalized[:-1] + "+00:00"
        return datetime.fromisoformat(normalized).timestamp()
    except ValueError:
        return None


def sort_history_by_time(replica_history: List[Dict]) -> Tuple[List[Dict], List[float]]:
    timestamped_history = []
    for replica in replica_history:
        ts = parse_time(replica.get("time"))
        if ts is None:
            raise ValueError("invalid or missing RFC3339 timestamp in replicaHistory entry")
        timestamped_history.append((ts, replica))

    timestamped_history.sort(key=lambda entry: entry[0])
    sorted_history = [entry[1] for entry in timestamped_history]
    timestamps = [entry[0] for entry in timestamped_history]
    return sorted_history, timestamps


def calculate_median_interval_ms(timestamps: List[float]) -> int:
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
    median_interval_ms = calculate_median_interval_ms(timestamps)
    return max(1, int(round(look_ahead_ms / median_interval_ms)))


def normalize_config(raw: Dict) -> Dict:
    return {
        "order": raw.get("order"),
        "lookAhead": raw.get("lookAhead"),
        "trend": raw.get("trend"),
        "enforceStationarity": raw.get("enforceStationarity", True),
        "enforceInvertibility": raw.get("enforceInvertibility", True),
        "concentrateScale": raw.get("concentrateScale", False),
        "useSarima": raw.get("useSarima", False),
        "seasonalOrder": raw.get("seasonalOrder"),
        "seasonalPeriods": raw.get("seasonalPeriods"),
    }


def validate_config(config: Dict) -> None:
    order = config.get("order")
    if not order or len(order) != 3:
        raise ValueError("invalid ARIMA order; expected [p, d, q]")
    for idx, param in enumerate(order):
        if param < 0:
            raise ValueError(f"invalid ARIMA order parameter {idx}; must be non-negative")

    if config["lookAhead"] is None or int(config["lookAhead"]) <= 0:
        raise ValueError("invalid lookAhead; must be > 0")

    use_sarima = bool(config.get("useSarima"))
    seasonal_order = config.get("seasonalOrder")
    seasonal_periods = config.get("seasonalPeriods")
    if use_sarima or seasonal_order is not None or seasonal_periods is not None:
        if seasonal_order is None or len(seasonal_order) != 3:
            raise ValueError("invalid seasonalOrder; expected [P, D, Q]")
        if seasonal_periods is None or int(seasonal_periods) <= 0:
            raise ValueError("invalid seasonalPeriods; must be > 0")
        for idx, param in enumerate(seasonal_order):
            if param < 0:
                raise ValueError(f"invalid seasonalOrder parameter {idx}; must be non-negative")


def build_model(series: List[float], config: Dict):
    use_sarima = bool(config["useSarima"]) or config["seasonalOrder"] is not None or config["seasonalPeriods"] is not None
    if use_sarima:
        seasonal_order = tuple(config["seasonalOrder"] or [0, 0, 0])
        seasonal_periods = int(config["seasonalPeriods"] or 0)
        return sm.tsa.SARIMAX(
            series,
            order=tuple(config["order"]),
            seasonal_order=seasonal_order + (seasonal_periods,),
            trend=config["trend"],
            enforce_stationarity=bool(config["enforceStationarity"]),
            enforce_invertibility=bool(config["enforceInvertibility"]),
            concentrate_scale=bool(config["concentrateScale"]),
        )

    return sm.tsa.ARIMA(
        series,
        order=tuple(config["order"]),
        trend=config["trend"],
        enforce_stationarity=bool(config["enforceStationarity"]),
        enforce_invertibility=bool(config["enforceInvertibility"]),
        concentrate_scale=bool(config["concentrateScale"]),
    )


def fit_model(series: List[float], config: Dict):
    model = build_model(series, config)
    try:
        return model.fit(disp=False)
    except TypeError:
        return model.fit()


def forecast_prediction() -> int:
    if _state_result is None or _state_config is None:
        raise RuntimeError("worker state is not initialized")

    forecast_steps = determine_forecast_steps(int(_state_config["lookAhead"]), _state_timestamps)
    forecast = _state_result.forecast(steps=forecast_steps)
    prediction = math.ceil(float(forecast[-1])) if len(forecast) > 0 else int(_state_series[-1])

    return int(prediction)


def ensure_matching_config(config: Dict) -> None:
    if _state_config is None:
        raise RuntimeError("worker state is not initialized")
    if config != _state_config:
        raise ValueError("worker config mismatch, reset or refit is required")


def handle_fit_forecast(request: Dict) -> Dict:
    global _state_result, _state_series, _state_timestamps, _state_config

    config = normalize_config(request.get("config", {}))
    validate_config(config)

    history = request.get("replicaHistory", [])
    if len(history) < 3:
        raise ValueError("ARIMA requires at least 3 observations")

    sorted_history, timestamps = sort_history_by_time(history)
    series = [float(replica["replicas"]) for replica in sorted_history]

    _state_result = fit_model(series, config)
    _state_series = series
    _state_timestamps = timestamps
    _state_config = config

    return {
        "ok": True,
        "prediction": forecast_prediction(),
    }


def handle_append_forecast(request: Dict) -> Dict:
    global _state_result, _state_series, _state_timestamps

    if _state_result is None or _state_config is None:
        raise RuntimeError("worker state is not initialized")
    config = normalize_config(request.get("config", {}))
    validate_config(config)
    ensure_matching_config(config)

    new_history = request.get("replicaHistory", [])
    if len(new_history) > 0:
        sorted_history, timestamps = sort_history_by_time(new_history)
        new_series = [float(replica["replicas"]) for replica in sorted_history]

        _state_result = _state_result.extend(new_series)
        _state_series.extend(new_series)
        _state_timestamps.extend(timestamps)

    return {
        "ok": True,
        "prediction": forecast_prediction(),
    }


def handle_forecast(request: Dict) -> Dict:
    if _state_result is None:
        raise RuntimeError("worker state is not initialized")
    config = normalize_config(request.get("config", {}))
    validate_config(config)
    ensure_matching_config(config)
    return {"ok": True, "prediction": forecast_prediction()}


def handle_reset() -> Dict:
    global _state_result, _state_series, _state_timestamps, _state_config
    _state_result = None
    _state_series = []
    _state_timestamps = []
    _state_config = None
    return {"ok": True}


def handle_request(request: Dict) -> Dict:
    action = request.get("action")

    if action == "fit_forecast":
        return handle_fit_forecast(request)
    if action == "append_forecast":
        return handle_append_forecast(request)
    if action == "forecast":
        return handle_forecast(request)
    if action == "reset":
        return handle_reset()

    raise ValueError(f"unsupported action '{action}'")


def main() -> int:
    for line in sys.stdin:
        message = line.strip()
        if message == "":
            continue

        try:
            request = json.loads(message)
            response = handle_request(request)
        except Exception as ex:  # pylint: disable=broad-except
            response = {"ok": False, "error": str(ex)}

        sys.stdout.write(json.dumps(response) + "\n")
        sys.stdout.flush()

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
