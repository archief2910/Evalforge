"""Test fixture: fails the first attempt, succeeds afterwards.

Exercises the job retry path (jobs.max_attempts = 2): the experiment must end
up completed, not failed, and the retried callback must not duplicate runs.
"""
import os
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
real = os.environ.get("REAL_WORKER") or os.path.join(ROOT, "worker-py", "worker.py")
marker = os.environ.get("FLAKY_MARKER") or os.path.join(ROOT, "flaky.marker")

if not os.path.exists(marker):
    with open(marker, "w", encoding="utf-8") as f:
        f.write("failed once\n")
    print("fixture: deliberate first-attempt failure", flush=True)
    sys.exit(3)

sys.exit(subprocess.call([sys.executable, real, *sys.argv[1:]]))
