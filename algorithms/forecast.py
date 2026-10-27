"""Versioned one-shot CPU forecast dispatcher.

Model entrypoints retain their direct legacy CLI for existing callers. Production
Go invokes this dispatcher, which keeps model stdout inside a bounded buffer and
emits exactly one correlated JSON response. It does not launch another process.
"""
import contextlib
import io
import json
from pathlib import Path
import re
import runpy
import sys

# Keep imports working for direct execution, importlib tests and runpy callers.
sys.path.insert(0, str(Path(__file__).resolve().parent))
try:
    from forecast_diagnostics import capture, describe
    from forecast_json import MAX_INPUT, decode_request
    from forecast_budget import request_budget
    from forecast_input import (validate_training_input, validate_arima_configuration,
                                validate_tree_configuration, validate_holt_winters_configuration)
finally:
    sys.path.pop(0)

VERSION = 1
UNIT = "cpu_millicores"
MAX_MODEL_OUTPUT = 4096
MODELS = {
    "Linear": "linear_regression/linear_regression.py",
    "HoltWinters": "holt_winters/holt_winters.py",
    "ARIMA": "arima/arima.py",
    "XGBoost": "xgboost/xgboost.py",
    "LightGBM": "lightgbm/lightgbm.py",
}


class BoundedOutput(io.StringIO):
    def write(self, value):
        if self.tell() + len(value) > MAX_MODEL_OUTPUT:
            raise ValueError("model stdout exceeds forecast output limit")
        return super().write(value)


def validate_request(request):
    if not isinstance(request, dict):
        raise ValueError("forecast request must be an object")
    if set(request) - {"diagnosticsVersion"} != {"protocolVersion", "unit", "requestId", "model", "input"}:
        raise ValueError("forecast request has missing or unknown fields")
    if "diagnosticsVersion" in request and (type(request["diagnosticsVersion"]) is not int or request["diagnosticsVersion"] != 1):
        raise ValueError("unsupported diagnostics version")
    if type(request["protocolVersion"]) is not int or request["protocolVersion"] != VERSION:
        raise ValueError("unsupported forecast protocol version")
    if request["unit"] != UNIT:
        raise ValueError("unsupported forecast unit")
    request_id = request["requestId"]
    if not isinstance(request_id, str) or not request_id.strip() or len(request_id) > 128:
        raise ValueError("invalid forecast request ID")
    if not isinstance(request["model"], str) or request["model"] not in MODELS:
        raise ValueError("unsupported forecast model")
    if not isinstance(request["input"], dict):
        raise ValueError("forecast input must be an object")


def execute_model(model, payload):
    output = BoundedOutput()
    original_stdin = sys.stdin
    try:
        sys.stdin = io.StringIO(json.dumps(payload, allow_nan=False))
        with contextlib.redirect_stdout(output):
            try:
                runpy.run_path(str(Path(__file__).resolve().parent / MODELS[model]), run_name="__main__")
            except SystemExit as exc:
                if exc.code not in (None, 0):
                    raise ValueError("model exited unsuccessfully") from exc
    finally:
        sys.stdin = original_stdin
    raw = output.getvalue().strip()
    if payload.get("returnTrajectory") is True:
        result = json.loads(raw)
        if not isinstance(result, dict) or set(result) != {"prediction", "trajectory"} or not 1 <= len(result["trajectory"]) <= 32:
            raise ValueError("invalid timed forecast")
        return result
    if re.fullmatch(r"[+-]?[0-9]+", raw) is None:
        raise ValueError("model must return one integer CPU forecast")
    value = int(raw)
    if not -(2 ** 63) <= value < 2 ** 63:
        raise ValueError("model forecast is outside int64 range")
    return value


def response_for(request):
    response = {"protocolVersion": VERSION, "unit": UNIT, "ok": False}
    if isinstance(request, dict):
        request_id = request.get("requestId")
        if isinstance(request_id, str) and request_id.strip() and len(request_id) <= 128:
            response["requestId"] = request_id
    try:
        validate_request(request)
        validate_training_input(request["model"], request["input"])
        if request["model"] == "ARIMA":
            validate_arima_configuration(request["input"])
        elif request["model"] in ("XGBoost", "LightGBM"):
            validate_tree_configuration(request["model"], request["input"])
        elif request["model"] == "HoltWinters":
            validate_holt_winters_configuration(request["input"])
        request_budget(request["model"], request["input"])
        if request.get("diagnosticsVersion") == 1:
            diagnostics = describe(request["model"], request["input"], {"trainingNanoseconds": 0, "inferenceNanoseconds": 0})
            with capture() as timings:
                response["prediction"] = execute_model(request["model"], request["input"])
            diagnostics.update(timings)
            response["diagnostics"] = diagnostics
        else:
            response["prediction"] = execute_model(request["model"], request["input"])
        if isinstance(response["prediction"], dict):
            result = response.pop("prediction")
            response.update(result)
        response["ok"] = True
    except Exception as exc:  # A model failure is a failed forecast, never a numeric result.
        response.pop("prediction", None)
        response.pop("trajectory", None)
        response.pop("diagnostics", None)
        if isinstance(request, dict) and request.get("diagnosticsVersion") == 1:
            response["errorCode"] = "invalid_input" if isinstance(exc, (ValueError, KeyError, TypeError)) else "model_failed"
        response["error"] = (str(exc) or type(exc).__name__)[:1024]
    return response


def main():
    try:
        # Bound before decoding; Go independently limits the encoded request.
        raw = sys.stdin.read(MAX_INPUT + 1)
        response = response_for(decode_request(raw))
    except Exception as exc:
        response = {"protocolVersion": VERSION, "unit": UNIT, "ok": False,
                    "error": (str(exc) or type(exc).__name__)[:1024]}
    print(json.dumps(response, allow_nan=False, separators=(",", ":")))


if __name__ == "__main__":
    main()
