import io
import json
import math
import runpy
import sys
from decimal import Decimal
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent / "seasonal_naive.py"


def load_module():
    return runpy.run_path(str(SCRIPT), run_name="seasonal_naive_test_module")


def run(payload, capsys):
    stdin = sys.stdin
    sys.stdin = io.StringIO(json.dumps(payload))
    try:
        runpy.run_path(str(SCRIPT), run_name="__main__")
    finally:
        sys.stdin = stdin
    return capsys.readouterr().out


def history(values, step=30):
    return [{"time": "2026-11-11T10:%02d:%02dZ" % (step * i // 60, step * i % 60), "totalCpuUsageMillicores": v}
            for i, v in enumerate(values)]


def test_repeats_the_point_one_cycle_earlier():
    module = load_module()
    times = [Decimal(30 * i) for i in range(12)]
    values = [float(v) for v in [1, 2, 3, 4] * 3]
    forecast, period = module["seasonal_naive"](times, values, 3, period=120)
    assert period == 120 and forecast == [1.0, 2.0, 3.0]


def test_without_a_cycle_repeats_the_newest_sample():
    module = load_module()
    times = [Decimal(30 * i) for i in range(5)]
    forecast, period = module["seasonal_naive"](times, [5.0, 1.0, 9.0, 2.0, 7.0], 3)
    assert period is None and forecast == [7.0, 7.0, 7.0]


def test_finds_the_cycle_from_history():
    module = load_module()
    times = [Decimal(30 * i) for i in range(60)]
    values = [1000 + 500 * math.sin(2 * math.pi * i / 10) for i in range(60)]
    forecast, period = module["seasonal_naive"](times, values, 3)
    assert abs(period - 300) <= 5
    assert [round(v) for v in forecast] == [round(values[50]), round(values[51]), round(values[52])]


def test_cli_emits_trajectory_and_scalar(capsys):
    payload = {"lookAhead": 90000, "returnTrajectory": True, "seasonalPeriodSeconds": 120, "replicaHistory": history([100, 200, 300, 400] * 3)}
    result = json.loads(run(payload, capsys))
    assert result["prediction"] == 300 and [p["cpu"] for p in result["trajectory"]] == [100, 200, 300]
    payload.update(returnTrajectory=False)
    assert run(payload, capsys) == "300"
