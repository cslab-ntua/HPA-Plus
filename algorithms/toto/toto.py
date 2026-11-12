"""Persistent Toto 2.0 worker: one newline-delimited JSON request per forecast.

The pretrained model is loaded on the first forecast and kept for the life of the
process, so later forecasts skip the slow PyTorch and model imports. It never fits.
Toto's own packages live in a separate folder, so its older pandas never reaches the
other models.
"""
import json
import math
import os
import sys

TOTO_SITE = os.environ.get("HPAPLUS_TOTO_SITE", "/app/toto-site")

from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
try:
    from forecast_diagnostics import capture, describe, stage
    from forecast_json import decode_request as decode_json, read_message
    from forecast_time import forecast_steps, parse_timestamp
    from forecast_trajectory import grid, result as trajectory_result
finally:
    sys.path.pop(0)

PROTOCOL_VERSION = 1
PROTOCOL_UNIT = "cpu_millicores"
MODEL_PATH = os.environ.get("HPAPLUS_TOTO_MODEL", "/app/models/toto-2.0-22m")
MAX_CONTEXT = 2048
MAX_STEPS = 64
QUANTILES = [0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9]

_pipeline = None


class TotoPipeline:
    """Feeds one CPU series to Toto and returns the requested quantile for each step."""

    def __init__(self, model, torch):
        self.model, self.torch = model, torch
        self.patch_size = model.config.patch_size

    def predict(self, values, steps, quantile):
        torch = self.torch
        # Toto reads whole patches: pad the front and mark the padding as missing.
        pad = (-len(values)) % self.patch_size
        target = torch.tensor([0.0] * pad + values, dtype=torch.float32).reshape(1, 1, -1)
        mask = torch.tensor([False] * pad + [True] * len(values)).reshape(1, 1, -1)
        inputs = {"target": target, "target_mask": mask, "series_ids": torch.zeros(1, 1, dtype=torch.long)}
        with torch.no_grad():
            quantiles = self.model.forecast(inputs, steps)
        return [float(v) for v in quantiles[QUANTILES.index(quantile)].reshape(-1)[:steps]]


def load_pipeline():
    """Import PyTorch and Toto only here, so tests and other models never pay for them."""
    global _pipeline
    if _pipeline is None:
        # The weights ship in the image; never reach the network, and keep any cache in /tmp.
        os.environ.setdefault("HF_HUB_OFFLINE", "1")
        os.environ.setdefault("HF_HOME", "/tmp/hpaplus-huggingface")
        os.environ.setdefault("MPLCONFIGDIR", "/tmp/hpaplus-matplotlib")
        sys.path.insert(0, TOTO_SITE)
        import warnings
        warnings.filterwarnings("ignore")
        import torch
        from toto2 import Toto2Model
        torch.set_num_threads(1)
        _pipeline = TotoPipeline(Toto2Model.from_pretrained(MODEL_PATH).eval(), torch)
    return _pipeline


def decode_request(request):
    if not isinstance(request, dict):
        raise ValueError("worker request must be an object")
    if type(request.get("protocolVersion")) is not int or request["protocolVersion"] != PROTOCOL_VERSION:
        raise ValueError("unsupported worker protocol version")
    if request.get("unit") != PROTOCOL_UNIT:
        raise ValueError("unsupported worker metric unit")
    request_id = request.get("requestId")
    if not isinstance(request_id, str) or not request_id.strip() or len(request_id) > 128:
        raise ValueError("requestId must be a nonempty string of at most 128 characters")
    if request.get("action") != "forecast":
        raise ValueError("unsupported worker action")
    if "diagnosticsVersion" in request and (type(request["diagnosticsVersion"]) is not int or request["diagnosticsVersion"] != 1):
        raise ValueError("unsupported diagnostics version")
    if set(request) - {"protocolVersion", "unit", "requestId", "action", "diagnosticsVersion", "config", "replicaHistory"}:
        raise ValueError("unknown fields for worker action")
    config = request.get("config")
    if not isinstance(config, dict) or set(config) - {"lookAhead", "quantileLevel", "returnTrajectory"}:
        raise ValueError("config must hold lookAhead, quantileLevel and returnTrajectory")
    if type(config.get("lookAhead")) is not int or config["lookAhead"] <= 0:
        raise ValueError("lookAhead must be a positive integer in milliseconds")
    quantile = config.get("quantileLevel")
    if type(quantile) not in (int, float) or not any(abs(quantile - q) < 1e-9 for q in QUANTILES):
        raise ValueError("quantileLevel must be one of 0.1, 0.2, ..., 0.9")
    if type(config.get("returnTrajectory", False)) is not bool:
        raise ValueError("returnTrajectory must be a boolean")
    history = request.get("replicaHistory")
    if not isinstance(history, list) or not 2 <= len(history) <= MAX_CONTEXT:
        raise ValueError("replicaHistory must hold between 2 and %d samples" % MAX_CONTEXT)
    rows = []
    for sample in history:
        if not isinstance(sample, dict):
            raise ValueError("history samples must be objects")
        usage = sample.get("totalCpuUsageMillicores")
        if type(usage) is not int or not 0 <= usage <= 2**63 - 1:
            raise ValueError("CPU history must contain nonnegative int64 millicores")
        rows.append((parse_timestamp(sample.get("time")), sample["time"], usage))
    rows.sort(key=lambda row: row[0])
    if any(b[0] <= a[0] for a, b in zip(rows, rows[1:])):
        raise ValueError("history timestamps must be distinct")
    return request_id, config, rows


def forecast(config, rows, pipeline):
    """Returns the quantile forecast for every step up to the horizon, in millicores."""
    timestamps = [row[0] for row in rows]
    steps = forecast_steps(config["lookAhead"], timestamps)
    if steps > MAX_STEPS:
        raise ValueError("Toto forecasts at most %d steps" % MAX_STEPS)
    if config.get("returnTrajectory"):
        grid([row[1] for row in rows], config["lookAhead"])
    quantile = next(q for q in QUANTILES if abs(config["quantileLevel"] - q) < 1e-9)
    values = [max(0.0, v) for v in pipeline.predict([float(row[2]) for row in rows], steps, quantile)]
    if len(values) != steps or not all(math.isfinite(v) for v in values):
        raise ValueError("Toto returned an invalid forecast")
    return values


def handle(request, pipeline_loader=load_pipeline):
    request_id, config, rows = decode_request(request)
    with capture() as timings:
        with stage("inference"):
            values = forecast(config, rows, pipeline_loader())
    prediction = int(math.ceil(values[-1]))
    response = {"protocolVersion": PROTOCOL_VERSION, "unit": PROTOCOL_UNIT, "requestId": request_id, "ok": True}
    if config.get("returnTrajectory"):
        response.update(trajectory_result(prediction, values, [row[1] for row in rows], config["lookAhead"], math.ceil))
    else:
        response["prediction"] = prediction
    if request.get("diagnosticsVersion") == 1:
        diagnostics = describe("Toto", {"lookAhead": config["lookAhead"], "replicaHistory": [{"time": row[1]} for row in rows]},
                               dict(timings), action="forecast")
        response["diagnostics"] = diagnostics
    return response


def failure(request, error):
    response = {"ok": False, "error": str(error)[:1024]}
    request_id = request.get("requestId") if isinstance(request, dict) else None
    response.update(protocolVersion=PROTOCOL_VERSION, unit=PROTOCOL_UNIT,
                    requestId=request_id if isinstance(request_id, str) and len(request_id) <= 128 else "")
    if isinstance(request, dict) and request.get("diagnosticsVersion") == 1:
        response["errorCode"] = "invalid_input" if isinstance(error, (ValueError, KeyError, TypeError)) else "model_failed"
    return response


def main(stdin=sys.stdin, stdout=sys.stdout, pipeline_loader=load_pipeline):
    while True:
        request = None
        try:
            message = read_message(stdin)
            if message is None:
                break
            if not message.strip():
                continue
            request = decode_json(message)
            response = handle(request, pipeline_loader)
        except Exception as ex:  # pylint: disable=broad-except
            response = failure(request, ex)
        stdout.write(json.dumps(response, allow_nan=False, separators=(",", ":")) + "\n")
        stdout.flush()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
