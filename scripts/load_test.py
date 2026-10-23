"""Concurrent load benchmark against a real server and a temp database.

Measures what the WAL change was for: many writers and readers hitting one
SQLite file over HTTP. Reports throughput and p50/p95 latencies, and asserts
the stored data has no duplicates or losses.

Usage:  python scripts/load_test.py [--traces 360] [--threads 6]
Exit code 0 = all invariants held (counts exact, no non-2xx responses).
"""
import argparse
import json
import os
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ROOT_DIR = ROOT


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


def request(base, method, path, payload=None, timeout=30):
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(base + path, data=data, method=method,
                                headers={"Content-Type": "application/json"} if data else {})
    start = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            body = r.read().decode()
            status = r.status
    except urllib.error.HTTPError as e:
        body = e.read().decode(errors="replace")
        status = e.code
    return status, body, (time.perf_counter() - start) * 1000


def start_server():
    data_dir = tempfile.mkdtemp(prefix="aieval-load-")
    port = free_port()
    env = dict(os.environ)
    env.update({"DATA_DIR": data_dir, "ADDR": f":{port}", "OPENROUTER_API_KEY": ""})
    log_path = os.path.join(data_dir, "server.log")
    logf = open(log_path, "w", encoding="utf-8", errors="replace")
    proc = subprocess.Popen([server_binary()], cwd=os.path.join(ROOT, "server-go"), env=env,
                            stdout=logf, stderr=subprocess.STDOUT, text=True)
    base = f"http://localhost:{port}"
    deadline = time.time() + 30
    while time.time() < deadline:
        if proc.poll() is not None:
            logf.close()
            raise SystemExit("server exited during startup:\n" + open(log_path, encoding="utf-8", errors="replace").read())
        try:
            with urllib.request.urlopen(base + "/health", timeout=2) as r:
                if json.loads(r.read().decode()).get("ok"):
                    return proc, base, data_dir, logf
        except Exception:
            time.sleep(0.3)
    proc.kill()
    raise SystemExit("server did not become healthy in time")


def pct(values, p):
    if not values:
        return 0.0
    s = sorted(values)
    k = min(len(s) - 1, max(0, int(round((len(s) - 1) * p))))
    return s[k]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--traces", type=int, default=360)
    ap.add_argument("--threads", type=int, default=6)
    args = ap.parse_args()

    proc, base, data_dir, logf = start_server()
    try:
        st, body, _ = request(base, "POST", "/api/v1/projects",
                              {"name": "load-test", "description": "benchmark"})
        pid = json.loads(body)["id"]

        per_thread = args.traces // args.threads
        ingest_ms, errors = [], []
        lock = threading.Lock()

        def ingest(worker):
            local = []
            for i in range(per_thread):
                now = int(time.time() * 1000)
                root = f"load-{worker}-{i}"
                events = {"events": [
                    {"id": root, "name": "load.agent", "type": "task",
                     "input": f"question {worker}-{i}", "start_ms": now},
                    {"id": root + "-t", "parent_id": root, "name": "load.tool", "type": "tool",
                     "input": {"q": i}, "output": {"doc": "kb"}, "start_ms": now + 1, "end_ms": now + 8},
                    {"id": root + "-l", "parent_id": root, "name": "load.llm", "type": "llm",
                     "model": "gpt-4o-mini", "input": f"q{i}", "output": f"a{i}",
                     "start_ms": now + 8, "end_ms": now + 40},
                ]}
                status, body, ms = request(base, "POST", f"/api/v1/projects/{pid}/spans:ingest", events)
                local.append(ms)
                if status != 200:
                    with lock:
                        errors.append((status, body[:200]))
            with lock:
                ingest_ms.extend(local)

        start = time.perf_counter()
        threads = [threading.Thread(target=ingest, args=(w,)) for w in range(args.threads)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        ingest_elapsed = time.perf_counter() - start
        total_requests = per_thread * args.threads
        spans_written = total_requests * 3

        # OTLP/HTTP concurrently, protobuf-free JSON path
        otlp_errors, otlp_ms = [], []
        otlp_batches = 60
        otlp_threads = 4

        def otlp(worker):
            local = []
            for i in range(otlp_batches // otlp_threads):
                trace = f"{worker:02x}{i:02x}" * 8
                root = f"{worker:02x}{i:02x}" * 4
                ns = int(time.time() * 1e9)
                payload = {"resourceSpans": [{"resource": {"attributes": [
                    {"key": "service.name", "value": {"stringValue": "load"}}]},
                    "scopeSpans": [{"spans": [
                        {"traceId": trace, "spanId": root, "name": "otlp.agent",
                         "kind": 1, "startTimeUnixNano": str(ns), "endTimeUnixNano": str(ns + 1_000_000)},
                    ]}]}]}
                data = json.dumps(payload).encode()
                req = urllib.request.Request(base + "/v1/traces", data=data, method="POST",
                                             headers={"Content-Type": "application/json",
                                                      "project-name": "otlp-load"})
                t0 = time.perf_counter()
                try:
                    with urllib.request.urlopen(req, timeout=20) as r:
                        if r.status != 200:
                            with lock:
                                otlp_errors.append(r.status)
                except urllib.error.HTTPError as e:
                    with lock:
                        otlp_errors.append(e.code)
                local.append((time.perf_counter() - t0) * 1000)
            with lock:
                otlp_ms.extend(local)

        threads = [threading.Thread(target=otlp, args=(w,)) for w in range(otlp_threads)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()

        # read-side latency under the accumulated data
        list_ms, dash_ms = [], []
        for _ in range(30):
            st, _, ms = request(base, "GET", f"/api/v1/projects/{pid}/traces?limit=50")
            list_ms.append(ms)
        for _ in range(10):
            st, _, ms = request(base, "GET", f"/api/v1/projects/{pid}/dashboard")
            dash_ms.append(ms)

        st, body, _ = request(base, "GET", f"/api/v1/projects/{pid}/traces?limit=1")
        total_traces = json.loads(body).get("total")
        st, body, _ = request(base, "GET", f"/api/v1/projects/{pid}/traces?limit=500")
        returned = len(json.loads(body).get("traces", []))

        print(f"concurrent native ingest: {total_requests} requests, {spans_written} spans, "
              f"{args.threads} threads")
        print(f"  wall {ingest_elapsed:.2f}s -> {spans_written / ingest_elapsed:,.0f} spans/s")
        print(f"  request p50 {pct(ingest_ms, 0.50):.1f} ms, p95 {pct(ingest_ms, 0.95):.1f} ms, "
              f"max {max(ingest_ms):.1f} ms")
        print(f"OTLP/HTTP JSON: {otlp_batches} batches, {otlp_threads} threads, "
              f"p95 {pct(otlp_ms, 0.95):.1f} ms, errors={len(otlp_errors)}")
        print(f"GET traces?limit=50: p50 {pct(list_ms, 0.50):.1f} ms, p95 {pct(list_ms, 0.95):.1f} ms")
        print(f"GET dashboard:       p50 {pct(dash_ms, 0.50):.1f} ms, p95 {pct(dash_ms, 0.95):.1f} ms")
        print(f"traces stored: {total_traces} (expected {total_requests}), "
              f"returned in page: {returned}")

        ok = (not errors and not otlp_errors and total_traces == total_requests)
        print("\n" + ("PASS" if ok else "FAIL") +
              f"  ingest_errors={len(errors)} otlp_errors={len(otlp_errors)} "
              f"traces={total_traces}/{total_requests}")
        if errors:
            print("first error:", errors[0])
        return 0 if ok else 1
    finally:
        try:
            proc.terminate()
            proc.wait(timeout=10)
        except Exception:
            proc.kill()
        logf.close()


if __name__ == "__main__":
    sys.exit(main())
