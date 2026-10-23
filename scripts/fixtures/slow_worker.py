"""Test fixture: a worker that stalls before delegating to the real worker.

Used by scripts/fault_test.py to create a window in which the server can be
hard-killed mid-evaluation, and to trigger the WORKER_TIMEOUT_SECONDS path.
"""
import os
import subprocess
import sys
import time

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
real = os.environ.get("REAL_WORKER") or os.path.join(ROOT, "worker-py", "worker.py")
delay = float(os.environ.get("WORKER_SLOW_SECONDS", "30"))
time.sleep(delay)
sys.exit(subprocess.call([sys.executable, real, *sys.argv[1:]]))
