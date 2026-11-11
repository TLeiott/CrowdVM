# crowdvm-evals

Boat's [evals / RL pattern](https://docs.boat.dev/evals) as a small library: prepare the environment once, save it as a
saved snapshot, start one sandbox per episode from it, score, delete.

```python
from crowdvm import CrowdVM
from crowdvm_evals import prepare_template, run_episodes, cleanup

cvm = CrowdVM()   # CROWDVM_API_KEY, CROWDVM_API_BASE
prepare_template(cvm, "task-env-v1", ["git clone https://github.com/acme/eval-tasks", "pip install -r eval-tasks/requirements.txt"])

def episode(ep, n):
    out = ep.exec(f"python3 eval-tasks/run.py --episode {n}", timeout_seconds=600)
    return n, out.exit_code, out.stdout

results = run_episodes(cvm, "task-env-v1", episode, range(100), max_workers=20, starts_per_minute=30)
cleanup(cvm)   # whatever a crashed run left behind
```

* `start_sandbox` retries 429 / 503 (Retry-After honoured) with the **same `Idempotency-Key`**, and deletes + retries a
  sandbox that comes up in `error` with `out_of_capacity` (a node that filled up between placement and boot).
* Episode sandboxes are `noEnv` (none of your secrets), have a TTL, and carry the label `crowdvm.eval=<run id>`.
* `Episode.fork()` branches mid-episode (stop the source first for an exact copy); `Episode.cost()` reads
  `GET /sandboxes/{id}/usage`.
* A saved snapshot never changes: use one per environment version. `prepare_template(..., replace=True)` re-saves a name.

`run_episodes_example.py` is a runnable end-to-end example. Tests: `python3 -m unittest discover -s tests -t .`
