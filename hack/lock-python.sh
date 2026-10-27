#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
python -c 'import sys; assert sys.version_info[:2] == (3, 14), "use Python 3.14"'
export CUSTOM_COMPILE_COMMAND='bash hack/lock-python.sh'
python -m piptools compile --generate-hashes --strip-extras \
  --output-file=algorithms/requirements.txt algorithms/requirements.in
