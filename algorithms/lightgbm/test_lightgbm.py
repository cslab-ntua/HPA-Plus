import json
import runpy
import subprocess
import sys
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent / "lightgbm.py"


def run_algo(payload: dict) -> subprocess.CompletedProcess:
    data = json.dumps(payload).encode()
    return subprocess.run(
        [sys.executable, str(SCRIPT)],
        input=data,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )


def with_cpu_usage(payload: dict) -> dict:
    updated = json.loads(json.dumps(payload))
    for entry in updated.get("replicaHistory", []):
        if "totalCpuUsageMillicores" not in entry and "replicas" in entry:
            entry["totalCpuUsageMillicores"] = entry["replicas"]
    return updated


def load_module() -> dict:
    return runpy.run_path(str(SCRIPT), run_name="lightgbm_test_module")


def test_falls_back_when_not_enough_history():
    payload = {
        "lookAhead": 1000,
        "lags": 4,
        "replicaHistory": [
            {"time": "2023-01-01T00:00:00Z", "replicas": 3},
            {"time": "2023-01-01T00:00:01Z", "replicas": 4},
        ],
    }
    result = run_algo(with_cpu_usage(payload))
    assert result.returncode == 0, result.stderr.decode()
    assert result.stdout.decode().strip() == "4"


def test_runs_and_outputs_integer_prediction():
    payload = {
        "lookAhead": 1000,
        "lags": 3,
        "replicaHistory": [
            {"time": "2023-01-01T00:00:00Z", "replicas": 2},
            {"time": "2023-01-01T00:00:01Z", "replicas": 3},
            {"time": "2023-01-01T00:00:02Z", "replicas": 4},
            {"time": "2023-01-01T00:00:03Z", "replicas": 5},
            {"time": "2023-01-01T00:00:04Z", "replicas": 6},
        ],
    }
    result = run_algo(with_cpu_usage(payload))
    assert result.returncode == 0, result.stderr.decode()
    output = result.stdout.decode().strip()
    assert output.isdigit(), f"expected integer output, got {output!r}"


def test_fails_when_cpu_usage_missing():
    payload = {
        "lookAhead": 1000,
        "lags": 2,
        "replicaHistory": [
            {"time": "2023-01-01T00:00:00Z", "replicas": 2},
            {"time": "2023-01-01T00:00:01Z", "replicas": 3},
            {"time": "2023-01-01T00:00:02Z", "replicas": 4},
        ],
    }
    result = run_algo(payload)
    assert result.returncode != 0
    assert "Missing totalCpuUsageMillicores" in result.stderr.decode()


def test_runs_when_window_size_is_smaller_than_lags():
    payload = {
        "lookAhead": 1000,
        "lags": 4,
        "windowSize": 2,
        "replicaHistory": [
            {"time": "2023-01-01T00:00:00Z", "replicas": 2},
            {"time": "2023-01-01T00:00:01Z", "replicas": 3},
            {"time": "2023-01-01T00:00:02Z", "replicas": 4},
            {"time": "2023-01-01T00:00:03Z", "replicas": 5},
            {"time": "2023-01-01T00:00:04Z", "replicas": 6},
            {"time": "2023-01-01T00:00:05Z", "replicas": 7},
        ],
    }
    result = run_algo(with_cpu_usage(payload))
    assert result.returncode == 0, result.stderr.decode()
    assert result.stdout.decode().strip().isdigit()


def test_imports_external_lightgbm_package():
    module = load_module()
    imported_path = Path(module["lgb"].__file__).resolve()
    assert imported_path != SCRIPT.resolve()
    assert SCRIPT.resolve() not in imported_path.parents


def test_build_model_params_preserves_zero_regularization():
    module = load_module()
    algorithm_input = module["AlgorithmInput"](
        look_ahead=1000,
        lags=2,
        replica_history=[],
        reg_lambda=0.0,
        reg_alpha=0.0,
        subsample=1.0,
        colsample_bytree=1.0,
    )
    params = module["build_model_params"](algorithm_input)
    assert params["reg_lambda"] == 0.0
    assert params["reg_alpha"] == 0.0


def test_runs_with_unbounded_max_depth():
    payload = {
        "lookAhead": 1000,
        "lags": 3,
        "maxDepth": -1,
        "replicaHistory": [
            {"time": "2023-01-01T00:00:00Z", "replicas": 2},
            {"time": "2023-01-01T00:00:01Z", "replicas": 3},
            {"time": "2023-01-01T00:00:02Z", "replicas": 4},
            {"time": "2023-01-01T00:00:03Z", "replicas": 5},
            {"time": "2023-01-01T00:00:04Z", "replicas": 6},
        ],
    }
    result = run_algo(with_cpu_usage(payload))
    assert result.returncode == 0, result.stderr.decode()
    assert result.stdout.decode().strip().isdigit()
