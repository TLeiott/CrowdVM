#!/bin/sh
# Unit tests of the SDKs and all adapters (no network, no server).
set -e
cd "$(dirname "$0")/.."
(cd sdk/python && PYTHONPATH=src python3 -m unittest discover -s tests -t . 2>&1 | tail -3)
(cd sdk/typescript && npm test 2>&1 | grep -E '^ℹ (tests|pass|fail)')
for d in harbor evals agentenv; do
  (cd integrations/$d && echo "== $d" && python3 -m unittest discover -s tests -t . 2>&1 | tail -3)
done
(cd integrations/eve && echo "== eve" && npm test 2>&1 | grep -E '^ℹ (tests|pass|fail)')
