import json
import subprocess
import sys
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent / "xgboost.py"


def run_algo(payload: dict) -> subprocess.CompletedProcess:
    data = json.dumps(payload).encode()
    return subprocess.run(
        [sys.executable, str(SCRIPT)],
        input=data,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )


def test_falls_back_when_not_enough_history():
    payload = {
        "lookAhead": 1000,
        "lags": 4,
        "replicaHistory": [
            {"time": "2023-01-01T00:00:00Z", "replicas": 3},
            {"time": "2023-01-01T00:00:01Z", "replicas": 4},
        ],
    }
    result = run_algo(payload)
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
    result = run_algo(payload)
    assert result.returncode == 0, result.stderr.decode()
    output = result.stdout.decode().strip()
    assert output.isdigit(), f"expected integer output, got {output!r}"
