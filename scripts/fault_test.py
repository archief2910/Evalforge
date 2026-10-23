"""Fault-injection tests for the evaluation job queue.

Three failure modes, each against a real server process and a real Python
worker:

1. crash        — hard-kill the server mid-evaluation, restart on the same
                  database: the job must be requeued and the experiment must
                  finish (not stay "running" forever).
2. timeout      — a worker that hangs must be killed at WORKER_TIMEOUT_SECONDS
                  and the experiment must land "failed" with a reason.
3. retry        — a worker that exits non-zero once must be retried, and the
                  retry must not duplicate runs (idempotent callback).

Usage:  python scripts/fault_test.py
Exit code 0 = every scenario passed.
"""
import json
import os
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FIXTURES = os.path.join(ROOT, "scripts", "fixtures")
REAL_WORKER = os.path.join(ROOT, "worker-py", "worker.py")
results = []


def check(name, ok, detail=""):
    results.append((name, ok, detail))
    print(f"{'PASS' if ok else 'FAIL'}  {name}" + (f"  -- {detail}" if detail else ""))
    return ok


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def server_binary():
    for name in ("evalforge.exe", "evalforge"):
        p = os.path.join(ROOT, "server-go", name)
        if os.path.exists(p):
            return p
    raise SystemExit("build the server first: (cd server-go && go build -o evalforge.exe .)")


def hard_kill(proc):
    """Simulate a power loss: kill the server *and* its worker children."""
    if os.name == "nt":
        subprocess.call(["taskkill", "/T", "/F", "/PID", str(proc.pid)],
                        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    else:
        proc.kill()
    try:
        proc.wait(timeout=10)
    except Exception:
        pass


def start_server(data_dir, port, extra_env=None, log_name="server.log"):
    env = dict(os.environ)
    env.update({
        "DATA_DIR": data_dir,
        "ADDR": f":{port}",
        "OPENROUTER_API_KEY": "",   # keep every run offline and deterministic
        "WORKER_PATH": os.path.join(FIXTURES, "slow_worker.py"),
        "REAL_WORKER": REAL_WORKER,
        "JOB_LEASE_SECONDS": "5",
    })
    env.update(extra_env or {})
    log_path = os.path.join(data_dir, log_name)
    log_file = open(log_path, "a", encoding="utf-8", errors="replace")
    proc = subprocess.Popen(
        [server_binary()], cwd=os.path.join(ROOT, "server-go"), env=env,
        stdout=log_file, stderr=subprocess.STDOUT, text=True,
    )
    base = f"http://localhost:{port}"
    deadline = time.time() + 30
    while time.time() < deadline:
        if proc.poll() is not None:
            log_file.close()
            with open(log_path, encoding="utf-8", errors="replace") as f:
                raise SystemExit(f"server exited during startup:\n{f.read()}")
        try:
            with urllib.request.urlopen(base + "/health", timeout=2) as r:
                if json.loads(r.read().decode()).get("ok"):
                    return proc, base, log_path, log_file
        except Exception:
            time.sleep(0.3)
    proc.kill()
    raise SystemExit("server did not become healthy in time")


def stop_server(proc, log_file):
    try:
        proc.terminate()
        proc.wait(timeout=10)
    except Exception:
        hard_kill(proc)
    try:
        log_file.close()
    except Exception:
        pass


def api(base, method, path, payload=None, timeout=20):
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(base + path, data=data, method=method,
                                headers={"Content-Type": "application/json"} if data else {})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        body = e.read().decode(errors="replace")
        try:
            return e.code, json.loads(body or "{}")
        except json.JSONDecodeError:
            return e.code, {"raw": body}


def wait_for(fn, timeout, interval=0.4):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            v = fn()
        except Exception:
            v = None
        if v:
            return v
        time.sleep(interval)
    return None


def db_conn(data_dir):
    return sqlite3.connect(os.path.join(data_dir, "ai_eval.db"), timeout=10)


def make_project_and_dataset(base, records=3):
    st, proj = api(base, "POST", "/api/v1/projects", {"name": f"fault-{time.time_ns()}"})
    pid = proj.get("id")
    st, ds = api(base, "POST", f"/api/v1/projects/{pid}/datasets",
                 {"name": "fault-dataset", "description": "fault injection"})
    dsid = ds.get("id")
    payload = [{"input": f"question {i}", "expected": f"answer {i}"} for i in range(records)]
    api(base, "POST", f"/api/v1/datasets/{dsid}/records", payload)
    return pid, dsid


def start_eval(base, pid, dsid, name):
    st, exp = api(base, "POST", f"/api/v1/projects/{pid}/evaluations",
                  {"dataset_id": dsid, "experiment_name": name,
                   "scorer_names": ["exact_match", "contains"]})
    return exp.get("experiment_id")


def scenario_crash_recovery():
    print("\n=== 1. crash mid-evaluation -> restart -> resumes ===")
    data_dir = tempfile.mkdtemp(prefix="aieval-crash-")
    port = free_port()
    proc = logf = None
    try:
        proc, base, log_path, logf = start_server(
            data_dir, port, {"WORKER_SLOW_SECONDS": "20"})
        pid, dsid = make_project_and_dataset(base)
        expid = start_eval(base, pid, dsid, "crash-recovery")
        check("eval enqueued", bool(expid), f"exp={expid}")

        running = wait_for(lambda: api(base, "GET", "/health")[1].get("jobs", {}).get("running") == 1, 20)
        check("dispatcher claimed the job", bool(running))
        # give the worker subprocess a moment to actually start
        time.sleep(1.0)

        hard_kill(proc)
        logf.close()
        check("server hard-killed (SIGKILL equivalent)", proc.poll() is not None)
        time.sleep(0.5)  # let the OS release the listening socket

        with db_conn(data_dir) as c:
            exp_status = c.execute("SELECT status FROM experiments WHERE id=?", (expid,)).fetchone()[0]
            job_status = c.execute("SELECT status FROM jobs WHERE experiment_id=?", (expid,)).fetchone()[0]
        check("without a restart the experiment is stranded",
              exp_status == "running" and job_status == "running",
              f"experiment={exp_status} job={job_status}")

        proc2, base, log_path, logf2 = start_server(
            data_dir, port, {"WORKER_SLOW_SECONDS": "1"}, log_name="server-restart.log")
        proc, logf = proc2, logf2

        with open(log_path, encoding="utf-8", errors="replace") as f:
            log_text = f.read() + ""
        requeued = wait_for(lambda: "requeued orphaned jobs" in open(log_path, encoding="utf-8", errors="replace").read(), 10)
        check("restart requeues the orphaned job from the log", bool(requeued))

        final = wait_for(lambda: (lambda d: d if d.get("status") in ("completed", "failed") else None)(
            api(base, "GET", f"/api/v1/experiments/{expid}")[1]), 90)
        check("experiment reaches a terminal state after restart",
              bool(final) and final.get("status") == "completed",
              f"status={final.get('status') if final else 'timeout'}")
        if final:
            runs = final.get("runs", [])
            ids = {r.get("record_id") for r in runs}
            check("no duplicated or missing runs after recovery",
                  len(runs) == 3 and len(ids) == 3, f"runs={len(runs)} distinct_records={len(ids)}")
            check("recovered runs are scored", all(r.get("scores") for r in runs),
                  f"scored={sum(1 for r in runs if r.get('scores'))}/{len(runs)}")

        with db_conn(data_dir) as c:
            status, attempts = c.execute(
                "SELECT status, attempts FROM jobs WHERE experiment_id=?", (expid,)).fetchone()
        check("job is marked done (not left running)", status == "done", f"status={status} attempts={attempts}")
    finally:
        if proc:
            stop_server(proc, logf)


def scenario_timeout():
    print("\n=== 2. hung worker -> timeout -> failed with a reason ===")
    data_dir = tempfile.mkdtemp(prefix="aieval-timeout-")
    port = free_port()
    proc = logf = None
    try:
        proc, base, _, logf = start_server(
            data_dir, port,
            {"WORKER_SLOW_SECONDS": "120", "WORKER_TIMEOUT_SECONDS": "2"})
        pid, dsid = make_project_and_dataset(base)
        expid = start_eval(base, pid, dsid, "timeout")
        final = wait_for(lambda: (lambda d: d if d.get("status") in ("completed", "failed") else None)(
            api(base, "GET", f"/api/v1/experiments/{expid}")[1]), 40)
        check("experiment fails instead of hanging",
              bool(final) and final.get("status") == "failed",
              f"status={final.get('status') if final else 'timeout'}")
        err = json.dumps(final.get("summary") or {}) if final else ""
        check("failure reason names the timeout", "timed out" in err, err[:120])
        with db_conn(data_dir) as c:
            status = c.execute("SELECT status FROM jobs WHERE experiment_id=?", (expid,)).fetchone()[0]
        check("job is failed, not stuck running", status == "failed", f"status={status}")
    finally:
        if proc:
            stop_server(proc, logf)


def scenario_retry():
    print("\n=== 3. worker exits non-zero once -> retried, no duplicate runs ===")
    data_dir = tempfile.mkdtemp(prefix="aieval-retry-")
    port = free_port()
    marker = os.path.join(data_dir, "flaky.marker")
    proc = logf = None
    try:
        proc, base, _, logf = start_server(
            data_dir, port,
            {"WORKER_PATH": os.path.join(FIXTURES, "flaky_worker.py"),
             "FLAKY_MARKER": marker, "WORKER_TIMEOUT_SECONDS": "60"})
        pid, dsid = make_project_and_dataset(base)
        expid = start_eval(base, pid, dsid, "retry")
        final = wait_for(lambda: (lambda d: d if d.get("status") in ("completed", "failed") else None)(
            api(base, "GET", f"/api/v1/experiments/{expid}")[1]), 90)
        check("first-attempt failure is retried to completion",
              bool(final) and final.get("status") == "completed",
              f"status={final.get('status') if final else 'timeout'}")
        check("fixture actually failed once", os.path.exists(marker))
        if final:
            runs = final.get("runs", [])
            check("retry did not duplicate runs", len(runs) == 3, f"runs={len(runs)}")
        with db_conn(data_dir) as c:
            status, attempts = c.execute(
                "SELECT status, attempts FROM jobs WHERE experiment_id=?", (expid,)).fetchone()
        check("job records two attempts", status == "done" and attempts == 2,
              f"status={status} attempts={attempts}")
    finally:
        if proc:
            stop_server(proc, logf)


def main():
    scenario_crash_recovery()
    scenario_timeout()
    scenario_retry()
    failed = [r for r in results if not r[1]]
    print(f"\n{len(results) - len(failed)}/{len(results)} checks passed")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
