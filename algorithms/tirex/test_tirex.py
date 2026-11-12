import io
import json
import runpy
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parent / "tirex.py"


def load_module():
    return runpy.run_path(str(SCRIPT), run_name="tirex_test_module")


class FakePipeline:
    def __init__(self, values):
        self.values = values
        self.calls = []

    def predict(self, values, steps, quantile):
        self.calls.append((list(values), steps, quantile))
        return list(self.values[:steps])


def request(history=None, **config):
    if history is None:
        history = [{"time": "2026-11-11T10:%02d:%02dZ" % (30 * i // 60, 30 * i % 60), "totalCpuUsageMillicores": 1000 + 100 * i}
                   for i in range(6)]
    body = {"lookAhead": 90000, "quantileLevel": 0.9, "returnTrajectory": True}
    body.update(config)
    return {"protocolVersion": 1, "unit": "cpu_millicores", "requestId": "r-1", "action": "forecast",
            "config": body, "replicaHistory": history}


def test_forecasts_trajectory_at_the_configured_quantile():
    module = load_module()
    pipeline = FakePipeline([1500.2, 1600.0, 1700.4])
    response = module["handle"](request(), lambda: pipeline)
    assert response["ok"] is True and response["prediction"] == 1701
    assert [p["cpu"] for p in response["trajectory"]] == [1501, 1600, 1701]
    assert response["trajectory"][0]["at"] == "2026-11-11T10:03:00Z"
    assert pipeline.calls[0] == ([1000.0, 1100.0, 1200.0, 1300.0, 1400.0, 1500.0], 3, 0.9)


def test_scalar_forecast_and_negative_values_clamped():
    module = load_module()
    response = module["handle"](request(returnTrajectory=False, quantileLevel=0.5), lambda: FakePipeline([-5.0, -1.0, -2.0]))
    assert response == {"protocolVersion": 1, "unit": "cpu_millicores", "requestId": "r-1", "ok": True, "prediction": 0}


def test_history_is_sorted_by_time():
    module = load_module()
    pipeline = FakePipeline([1.0, 1.0, 1.0])
    module["handle"](request(list(reversed(request()["replicaHistory"]))), lambda: pipeline)
    assert pipeline.calls[0][0][0] == 1000.0


@pytest.mark.parametrize("mutate", [
    lambda r: r.update(protocolVersion=2),
    lambda r: r.update(action="fit_forecast"),
    lambda r: r["config"].update(quantileLevel=0.95),
    lambda r: r["config"].update(quantileLevel=0.85),
    lambda r: r["config"].update(lookAhead=30 * 65 * 1000),
    lambda r: r.update(replicaHistory=r["replicaHistory"][:1]),
    lambda r: r["replicaHistory"][0].update(totalCpuUsageMillicores=-1),
])
def test_rejects_invalid_requests(mutate):
    module = load_module()
    body = request()
    mutate(body)
    with pytest.raises(ValueError):
        module["handle"](body, lambda: FakePipeline([1.0] * 70))


def test_pipeline_returns_the_requested_quantile_row():
    module = load_module()
    seen = {}

    class Torch:
        float32 = "float32"

        def tensor(self, values, dtype=None):
            return values

        class inference_mode:
            def __enter__(self):
                return None

            def __exit__(self, *args):
                return False

    def series(**fields):
        return fields

    class Model:
        def forecast(self, timeseries, prediction_length, output_type):
            seen.update(target=timeseries[0]["target"], steps=prediction_length, output_type=output_type)
            # One array per series, shaped (variates, quantiles, steps).
            return [[[[float(10 * q + s) for s in range(prediction_length)] for q in range(9)]]]

    pipeline = module["TiRexPipeline"](Model(), Torch(), series)
    assert pipeline.predict([5.0, 6.0, 7.0], 3, 0.9) == [80.0, 81.0, 82.0]
    assert pipeline.predict([5.0, 6.0, 7.0], 2, 0.1) == [0.0, 1.0]
    assert seen == {"target": [[5.0, 6.0, 7.0]], "steps": 2, "output_type": "numpy"}


def weights_file(tmp_path, tensors, metadata=True):
    """Writes a safetensors file: an 8-byte header length, a JSON header, then the raw bytes."""
    header, data = {}, b""
    if metadata:
        header["__metadata__"] = {"format": "pt"}
    for name, (dtype, shape, raw) in tensors.items():
        header[name] = {"dtype": dtype, "shape": shape, "data_offsets": [len(data), len(data) + len(raw)]}
        data += raw
    encoded = json.dumps(header).encode()
    path = tmp_path / "model.safetensors"
    path.write_bytes(len(encoded).to_bytes(8, "little") + encoded + data)
    return path


def test_read_weights_yields_each_tensor_and_skips_metadata(tmp_path):
    module = load_module()
    path = weights_file(tmp_path, {"a": ("F32", [2], b"\x01" * 8), "b": ("I64", [1, 1], b"\x02" * 8), "c": ("F32", [0], b"")})
    read = list(module["read_weights"](path, lambda data, dtype, shape: (bytes(data), dtype, shape)))
    assert read == [("a", (b"\x01" * 8, "F32", [2])), ("b", (b"\x02" * 8, "I64", [1, 1])), ("c", (b"", "F32", [0]))]


def test_read_weights_rejects_truncated_files(tmp_path):
    module = load_module()
    path = weights_file(tmp_path, {"a": ("F32", [2], b"\x01" * 8)})
    path.write_bytes(path.read_bytes()[:-1])
    with pytest.raises(ValueError, match="truncated"):
        list(module["read_weights"](path, lambda *args: args))
    path.write_bytes((2**40).to_bytes(8, "little"))
    with pytest.raises(ValueError, match="header"):
        list(module["read_weights"](path, lambda *args: args))


class Tensor:
    def __init__(self, shape, value=None):
        self.shape, self.value = shape, value

    def copy_(self, other):
        self.value = other.value


def test_load_weights_copies_every_weight_and_restores_aliases():
    module = load_module()
    shared = Tensor((2, 2))
    state = {"fwd.w": shared, "rev.w": shared, "head": Tensor((3,))}
    aliases = {"fwd.w": ("fwd.w", "rev.w"), "rev.w": ("fwd.w", "rev.w")}
    module["load_weights"](state, aliases, [("fwd.w", Tensor((2, 2), 7)), ("head", Tensor((3,), 9))])
    assert state["rev.w"].value == 7 and state["head"].value == 9


@pytest.mark.parametrize("weights, message", [
    ([("head", Tensor((3,), 1))], "missing"),
    ([("head", Tensor((4,), 1)), ("w", Tensor((2,), 1))], "shape"),
    ([("head", Tensor((3,), 1)), ("w", Tensor((2,), 1)), ("extra", Tensor((1,), 1))], "unexpected"),
])
def test_load_weights_is_strict(weights, message):
    module = load_module()
    with pytest.raises(ValueError, match=message):
        module["load_weights"]({"head": Tensor((3,)), "w": Tensor((2,))}, {}, weights)


def test_worker_loop_answers_every_line():
    module = load_module()
    pipeline = FakePipeline([10.0, 20.0, 30.0])
    good, bad = json.dumps(request()), json.dumps(request(quantileLevel=2))
    stdout = io.StringIO()
    module["main"](io.StringIO(good + "\n" + bad + "\n\n" + good + "\n"), stdout, lambda: pipeline)
    lines = [json.loads(line) for line in stdout.getvalue().splitlines()]
    assert [line["ok"] for line in lines] == [True, False, True]
