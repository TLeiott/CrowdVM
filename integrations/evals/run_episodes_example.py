"""Boat's evals recipe on CrowdVM: prepare a saved snapshot once, run N episodes in parallel, delete each sandbox.

    CROWDVM_API_KEY=cvm_... CROWDVM_API_BASE=http://<server>:8420/api/v1 python3 run_episodes_example.py 8
"""
import sys

from crowdvm import CrowdVM
from crowdvm_evals import cleanup, prepare_template, run_episodes

TEMPLATE = "task-env-v1"
cvm = CrowdVM()
prepare_template(cvm, TEMPLATE, ["mkdir -p eval-tasks && echo 'print(\"score\", 1)' > eval-tasks/run.py"], replace=True)


def episode(ep, n):
    out = ep.exec(f"python3 eval-tasks/run.py --episode {n}", timeout_seconds=60)
    return n, out.exit_code, out.stdout.strip()


try:
    for row in run_episodes(cvm, TEMPLATE, episode, range(int(sys.argv[1]) if len(sys.argv) > 1 else 4), max_workers=4):
        print(*row)
finally:
    print("left behind and deleted:", cleanup(cvm))
