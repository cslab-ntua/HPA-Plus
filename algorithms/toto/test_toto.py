import io
import json
import runpy
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parent / "toto.py"


def load_module():
    return runpy.run_path(str(SCRIPT), run_name="toto_test_module")


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


def test_pads_the_front_to_whole_patches_and_masks_the_padding():
    module = load_module()
    seen = {}

    class Tensor(list):
        def reshape(self, *shape):
            return self

    class Torch:
        float32, long = "float32", "long"

        def tensor(self, values, dtype=None):
            return Tensor(values)

        def zeros(self, *shape, dtype=None):
            return Tensor([0])

        class no_grad:
            def __enter__(self):
                return None

            def __exit__(self, *args):
                return False

    class Model:
        class config:
            patch_size = 4

        def forecast(self, inputs, steps):
            seen.update(inputs, steps=steps)
            return [Tensor([float(10 * q + s) for s in range(steps)]) for q in range(9)]

    pipeline = module["TotoPipeline"](Model(), Torch())
    assert pipeline.predict([5.0, 6.0, 7.0, 8.0, 9.0, 10.0], 3, 0.9) == [80.0, 81.0, 82.0]
    assert seen["target"] == [0.0, 0.0, 5.0, 6.0, 7.0, 8.0, 9.0, 10.0]
    assert seen["target_mask"] == [False, False, True, True, True, True, True, True] and seen["steps"] == 3


def test_worker_loop_answers_every_line():
    module = load_module()
    pipeline = FakePipeline([10.0, 20.0, 30.0])
    good, bad = json.dumps(request()), json.dumps(request(quantileLevel=2))
    stdout = io.StringIO()
    module["main"](io.StringIO(good + "\n" + bad + "\n\n" + good + "\n"), stdout, lambda: pipeline)
    lines = [json.loads(line) for line in stdout.getvalue().splitlines()]
    assert [line["ok"] for line in lines] == [True, False, True]
