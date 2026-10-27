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
from decimal import Decimal
import sys
import warnings
from typing import Dict, List, Optional, Tuple

# Resolve shared helpers for both direct script execution and runpy dispatch.
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
try:
    from forecast_trajectory import grid, result as trajectory_result
    from forecast_diagnostics import capture, describe, stage
    from forecast_time import parse_timestamp, median_interval_ms as shared_interval_ms, forecast_steps as shared_forecast_steps
    from forecast_budget import arima_budget
    from forecast_input import validate_arima_configuration
    from forecast_json import decode_request, read_message
finally:
    sys.path.pop(0)

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
_state_timestamps: List[Decimal] = []
_state_config: Optional[Dict] = None

PROTOCOL_VERSION = 1
PROTOCOL_UNIT = "cpu_millicores"


def validate_protocol(request: Dict) -> bool:
    """Legacy headerless requests remain supported; declared headers must match v1."""
    if not isinstance(request, dict):
        raise ValueError("worker request must be an object")
    if not any(key in request for key in ("protocolVersion", "unit", "requestId")):
        return False
    if type(request.get("protocolVersion")) is not int or request["protocolVersion"] != PROTOCOL_VERSION:
        raise ValueError("unsupported worker protocol version")
    if request.get("unit") != PROTOCOL_UNIT:
        raise ValueError("unsupported worker metric unit")
    request_id = request.get("requestId")
    if not isinstance(request_id, str) or not request_id.strip() or len(request_id) > 128:
        raise ValueError("requestId must be a nonempty string of at most 128 characters")
    action = request.get("action")
    if action not in ("fit_forecast", "append_forecast", "forecast", "reset"):
        raise ValueError("unsupported worker action")
    allowed = {"protocolVersion", "unit", "requestId", "action", "diagnosticsVersion"}
    if "diagnosticsVersion" in request and (type(request["diagnosticsVersion"]) is not int or request["diagnosticsVersion"] != 1):
        raise ValueError("unsupported diagnostics version")
    if action != "reset":
        allowed.add("config")
    if action in ("fit_forecast", "append_forecast"):
        allowed.add("replicaHistory")
    if set(request) - allowed:
        raise ValueError("unknown fields for worker action")
    return True


def validate_protocol_history(request: Dict) -> None:
    """Validate v1 samples before a fit/append can mutate the worker session."""
    action = request.get("action")
    if action not in ("fit_forecast", "append_forecast"):
        return
    history = request.get("replicaHistory", [])
    if not isinstance(history, list):
        raise ValueError("replicaHistory must be an array")
    previous = _state_timestamps[-1] if action == "append_forecast" and _state_timestamps else None
    for sample in history:
        if not isinstance(sample, dict):
            raise ValueError("history samples must be objects")
        raw_time = sample.get("time")
        timestamp = parse_time(raw_time)
        if timestamp is None or (previous is not None and timestamp <= previous):
            raise ValueError("history timestamps must be valid and strictly increasing")
        previous = timestamp
        usage = sample.get("totalCpuUsageMillicores")
        if type(usage) is not int or not 0 <= usage <= 2**63 - 1:
            raise ValueError("CPU history must contain nonnegative int64 millicores")


def protocol_response(request, response: Dict) -> Dict:
    if isinstance(request, dict) and any(key in request for key in ("protocolVersion", "unit", "requestId")):
        request_id = request.get("requestId")
        response.update(protocolVersion=PROTOCOL_VERSION, unit=PROTOCOL_UNIT,
                        requestId=request_id if isinstance(request_id, str) and len(request_id) <= 128 else "")
    return response


def parse_time(time_str):
    if time_str is None:
        return None
    try:
        return parse_timestamp(time_str)
    except ValueError:
        return None


def sort_history_by_time(replica_history: List[Dict]) -> Tuple[List[Dict], List[Decimal]]:
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


def calculate_median_interval_ms(timestamps):
    return shared_interval_ms(timestamps)


def determine_forecast_steps(look_ahead_ms, timestamps):
    return shared_forecast_steps(look_ahead_ms, timestamps)


def normalize_config(raw: Dict) -> Dict:
    return {
        "returnTrajectory": raw.get("returnTrajectory", False),
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
    if config.get("trend") not in (None, "n", "c", "t", "ct"):
        raise ValueError("invalid ARIMA trend")
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
        if seasonal_periods is None or int(seasonal_periods) < 2:
            raise ValueError("invalid seasonalPeriods; must be >= 2")
        for idx, param in enumerate(seasonal_order):
            if param < 0:
                raise ValueError(f"invalid seasonalOrder parameter {idx}; must be non-negative")
    elif (order[1] > 0 and config.get("trend") in ("c", "ct")) or (order[1] > 1 and config.get("trend") == "t"):
        raise ValueError("ARIMA trend degree must be at least the differencing order")


def build_model(series: List[float], config: Dict):
    arima_budget(len(series), config["order"], config["seasonalOrder"], config["seasonalPeriods"])
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


def extract_cpu_usage_series(replica_history: List[Dict]) -> List[float]:
    series = []
    for replica in replica_history:
        value = replica.get("totalCpuUsageMillicores")
        if value is None:
            raise ValueError("missing totalCpuUsageMillicores in replicaHistory entry")
        series.append(float(value))
    return series


def fit_model(series: List[float], config: Dict):
    model = build_model(series, config)
    try:
        return model.fit(disp=False)
    except TypeError:
        return model.fit()


def forecast_prediction(result, config, timestamps) -> int:
    if result is None or config is None:
        raise RuntimeError("worker state is not initialized")

    forecast_steps = determine_forecast_steps(int(config["lookAhead"]), timestamps)
    if config.get("returnTrajectory"):
        grid(timestamps, config["lookAhead"])
    with stage("inference"):
        forecast = result.forecast(steps=forecast_steps)
    if len(forecast) == 0:
        raise ValueError("ARIMA returned an empty forecast")
    prediction = math.ceil(float(forecast[-1]))
    if not -(2**63) <= prediction <= 2**63 - 1:
        raise ValueError("forecast must contain int64 millicores")

    if config.get("returnTrajectory"):
        return trajectory_result(prediction, forecast, timestamps, config["lookAhead"], math.ceil)
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

    arima_budget(len(history), config["order"], config["seasonalOrder"], config["seasonalPeriods"])
    sorted_history, timestamps = sort_history_by_time(history)
    determine_forecast_steps(int(config["lookAhead"]), timestamps)
    series = extract_cpu_usage_series(sorted_history)
    if config.get("returnTrajectory"):
        grid(timestamps, config["lookAhead"])
    with stage("training"):
        result = fit_model(series, config)
    prediction = forecast_prediction(result, config, timestamps)
    # Publish only a complete, usable fit. An unsuccessful forecast must not
    # silently replace a previously healthy session or advance its sample marker.
    _state_result = result
    _state_series = series
    _state_timestamps = timestamps
    _state_config = config

    return {"ok": True, **prediction} if isinstance(prediction, dict) else {"ok": True, "prediction": prediction}


def handle_append_forecast(request: Dict) -> Dict:
    global _state_result, _state_series, _state_timestamps

    if _state_result is None or _state_config is None:
        raise RuntimeError("worker state is not initialized")
    config = normalize_config(request.get("config", {}))
    validate_config(config)
    ensure_matching_config(config)

    new_history = request.get("replicaHistory", [])
    arima_budget(len(new_history), config["order"], config["seasonalOrder"], config["seasonalPeriods"])
    result, series, retained_times = _state_result, _state_series, _state_timestamps
    if len(new_history) > 0:
        sorted_history, timestamps = sort_history_by_time(new_history)
        new_series = extract_cpu_usage_series(sorted_history)
        # Statsmodels extend carries the fitted state forward without refitting.
        # Keep only a fit-window's worth of diagnostic values and timing samples,
        # so disabled periodic refits cannot grow Python metadata indefinitely.
        window = len(_state_series)
        series = (_state_series + new_series)[-window:]
        retained_times = (_state_timestamps + timestamps)[-window:]
        # A faster incoming cadence can exceed the step budget even with the
        # same lookAhead. Reject before extending a fitted model/session.
        determine_forecast_steps(int(config["lookAhead"]), retained_times)
        if config.get("returnTrajectory"):
            grid(retained_times, config["lookAhead"])
        with stage("training"):
            result = _state_result.extend(new_series)

    prediction = forecast_prediction(result, config, retained_times)
    _state_result, _state_series, _state_timestamps = result, series, retained_times
    return {"ok": True, **prediction} if isinstance(prediction, dict) else {"ok": True, "prediction": prediction}


def handle_forecast(request: Dict) -> Dict:
    if _state_result is None:
        raise RuntimeError("worker state is not initialized")
    config = normalize_config(request.get("config", {}))
    validate_config(config)
    ensure_matching_config(config)
    prediction = forecast_prediction(_state_result, _state_config, _state_timestamps)
    return {"ok": True, **prediction} if isinstance(prediction, dict) else {"ok": True, "prediction": prediction}


def handle_reset() -> Dict:
    global _state_result, _state_series, _state_timestamps, _state_config
    _state_result = None
    _state_series = []
    _state_timestamps = []
    _state_config = None
    return {"ok": True}


def _handle_request(request: Dict) -> Dict:
    versioned = validate_protocol(request)
    if versioned:
        if request.get("action") != "reset":
            validate_arima_configuration(request.get("config"), incremental=True)
        validate_protocol_history(request)
    action = request.get("action")

    if action == "fit_forecast":
        response = handle_fit_forecast(request)
    elif action == "append_forecast":
        response = handle_append_forecast(request)
    elif action == "forecast":
        response = handle_forecast(request)
    elif action == "reset":
        response = handle_reset()
    else:
        raise ValueError("unsupported worker action")
    if versioned and "prediction" in response:
        prediction = response["prediction"]
        if type(prediction) is not int or not -(2**63) <= prediction <= 2**63 - 1:
            raise ValueError("forecast must contain int64 millicores")
    return protocol_response(request, response)


def handle_request(request: Dict) -> Dict:
    # Build provenance before dispatch, so invalid metadata cannot publish a fit
    # or advance an append before the complete response is known to be usable.
    diagnostics = None
    if isinstance(request, dict) and request.get("diagnosticsVersion") == 1:
        versioned = validate_protocol(request)
        action = request.get("action")
        if action != "reset":
            if versioned:
                validate_arima_configuration(request.get("config"), incremental=True)
                validate_protocol_history(request)
            config = normalize_config(request.get("config", {}))
            if action == "fit_forecast":
                _, timestamps = sort_history_by_time(request.get("replicaHistory", []))
            elif action == "append_forecast":
                _, incoming = sort_history_by_time(request.get("replicaHistory", []))
                timestamps = (_state_timestamps + incoming)[-len(_state_series):]
            else:
                timestamps = _state_timestamps
            diagnostics = describe("ARIMA", config,
                                   {"trainingNanoseconds": 0, "inferenceNanoseconds": 0},
                                   action, timestamps)
    with capture() as timings:
        response = _handle_request(request)
    if diagnostics is not None and "prediction" in response:
        diagnostics.update(timings)
        response["diagnostics"] = diagnostics
    return response


def main() -> int:
    while True:
        request = None
        try:
            message = read_message(sys.stdin)
            if message is None:
                break
            if not message.strip():
                continue
            request = decode_request(message)
            response = handle_request(request)
        except Exception as ex:  # pylint: disable=broad-except
            response = protocol_response(request, {"ok": False, "error": str(ex)[:1024]})
            if isinstance(request, dict) and request.get("diagnosticsVersion") == 1:
                response["errorCode"] = "invalid_input" if isinstance(ex, (ValueError, KeyError, TypeError)) else "model_failed"

        sys.stdout.write(json.dumps(response, allow_nan=False) + "\n")
        sys.stdout.flush()

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
