#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
python -c 'import sys; assert sys.version_info[:2] == (3, 14), "use Python 3.14"'
export CUSTOM_COMPILE_COMMAND='bash hack/lock-python.sh'
for dir in algorithms docs; do
  python -m piptools compile --generate-hashes --strip-extras \
    --output-file=$dir/requirements.txt $dir/requirements.in
done
# Toto's own lock, installed with --no-deps into /app/toto-site by the image's toto target.
python -m piptools compile --generate-hashes --strip-extras --allow-unsafe \
  --output-file=algorithms/requirements-toto.txt algorithms/requirements-toto.in
