"""End-to-end smoke test for the Go+Python platform.

By default the script starts its own server instance on a free port with a
throw-away DATA_DIR, drives the whole /api/v1 contract against it (ingest,
traces, feedback, dashboard, datasets, prompts, eval dispatch to the Python
worker, experiment summary) and then shuts it down. The demo database is never
touched, and no provider key is passed in, so the run is fast and deterministic.

Usage:
    python scripts/smoke_api.py                 # isolated run (recommended)
    python scripts/smoke_api.py --base URL      # run against an existing server
Exit code 0 = every check passed, 1 = at least one failure.
"""
import argparse, json, os, shutil, socket, subprocess, sys, tempfile, time
import urllib.error, urllib.request

try:  # Windows consoles often default to cp1252 and choke on non-ascii output
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BASE = "http://localhost:8000"
results = []


def call(method, path, payload=None, timeout=30):
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(
        BASE + path, data=data, method=method,
        headers={"Content-Type": "application/json"} if data else {},
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, json.loads(resp.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        body = e.read().decode()
        try:
            return e.code, json.loads(body or "{}")
        except json.JSONDecodeError:
            return e.code, {"raw": body}


def post_raw(path, body, content_type="application/json", headers=None, timeout=20):
    req = urllib.request.Request(
        BASE + path, data=body, method="POST",
        headers={"Content-Type": content_type, **(headers or {})},
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read().decode(errors="replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(errors="replace")


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


def start_server():
    """Starts a private server on a temp database and waits until it answers."""
    data_dir = tempfile.mkdtemp(prefix="aieval-smoke-")
    port = free_port()
    env = dict(os.environ)
    env["DATA_DIR"] = data_dir
    env["ADDR"] = f":{port}"
    # keep the run offline: the server only passes through env vars it already
    # has, so an empty key stops .env from enabling real provider calls
    env["OPENROUTER_API_KEY"] = ""
    log_path = os.path.join(data_dir, "server.log")
    log_file = open(log_path, "w", encoding="utf-8", errors="replace")
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
                    return proc, base, data_dir
        except Exception:
            time.sleep(0.3)
    proc.kill()
    raise SystemExit("server did not become healthy in time")


def stop_server(proc, data_dir, verbose=False):
    try:
        proc.terminate()
        proc.wait(timeout=10)
    except Exception:
        proc.kill()
    if verbose:
        log_path = os.path.join(data_dir, "server.log")
        if os.path.exists(log_path):
            with open(log_path, encoding="utf-8", errors="replace") as f:
                tail = f.read()[-3000:]
            if tail.strip():
                print("\n--- server log ---\n" + tail)
    shutil.rmtree(data_dir, ignore_errors=True)


def main(base=None):
    global BASE
    proc, data_dir = None, None
    if base:
        BASE = base
    else:
        proc, BASE, data_dir = start_server()
        print(f"isolated server at {BASE}\n")
    code = 1
    try:
        code = run_checks()
    finally:
        if proc:
            stop_server(proc, data_dir, verbose=bool(code))
    return code


def run_checks():
    st, health = call("GET", "/health")
    check("health", st == 200 and health.get("ok") is True, f"status={st}")

    st, projects = call("GET", "/api/v1/projects")
    check("projects list", st == 200 and isinstance(projects, list), f"status={st} n={len(projects)}")

    st, proj = call("POST", "/api/v1/projects",
                    {"name": "smoke project", "description": "created by scripts/smoke_api.py"})
    pid = proj.get("id")
    check("project create", st == 200 and bool(pid), f"status={st}")
    if not pid:
        return 1

    st, one = call("GET", f"/api/v1/projects/{pid}")
    check("project detail", st == 200 and one.get("name") == "smoke project", f"status={st}")

    # ---- ingest a nested trace (root + tool + llm) ----
    now = int(time.time() * 1000)
    root, tool, llm = "smoke-root", "smoke-tool", "smoke-llm"
    st, ing = call("POST", f"/api/v1/projects/{pid}/spans:ingest", {"events": [
        {"id": root, "name": "agent", "type": "task", "input": "how do I reset my password?",
         "start_ms": now},
        {"id": tool, "parent_id": root, "name": "retrieve", "type": "tool",
         "input": {"q": "reset"}, "output": {"doc": "reset_password"},
         "start_ms": now + 10, "end_ms": now + 60},
        {"id": llm, "parent_id": root, "name": "answer", "type": "llm", "model": "gpt-4o-mini",
         "input": ["reset_password", "how do I reset my password?"],
         "output": "Click 'Forgot password' and follow the email link.",
         "start_ms": now + 60, "end_ms": now + 260},
    ]})
    check("spans ingest", st == 200 and ing.get("inserted") == 3 and ing.get("traces_created") == 1,
          f"status={st} {ing}")

    st, traces = call("GET", f"/api/v1/projects/{pid}/traces")
    tl = traces.get("traces", [])
    check("traces list", st == 200 and traces.get("total") == 1, f"total={traces.get('total')}")
    if tl:
        t = tl[0]
        check("trace aggregates model+cost from child spans",
              t.get("model") == "gpt-4o-mini" and t.get("cost_usd", 0) > 0 and t.get("span_count") == 3,
              f"model={t.get('model')} cost={t.get('cost_usd')} spans={t.get('span_count')}")

    st, detail = call("GET", f"/api/v1/projects/{pid}/traces/{root}")
    tree = detail.get("span_tree", [])
    kids = tree[0].get("children", []) if tree else []
    check("trace detail / nested span tree",
          st == 200 and len(tree) == 1 and len(kids) == 2, f"roots={len(tree)} children={len(kids)}")

    # ---- OTLP/HTTP ingest: the contract a real OpenTelemetry SDK uses ----
    trace_hex, otlp_root, otlp_llm = "1f" * 16, "0a" * 8, "0b" * 8
    base_ns = int(time.time() * 1e9)
    otlp_payload = json.dumps({
        "resourceSpans": [{
            "resource": {"attributes": [
                {"key": "service.name", "value": {"stringValue": "smoke-svc"}}]},
            "scopeSpans": [{
                "scope": {"name": "opentelemetry.instrumentation.openai"},
                "spans": [
                    {"traceId": trace_hex, "spanId": otlp_root, "name": "otlp-agent",
                     "kind": 1, "startTimeUnixNano": str(base_ns),
                     "endTimeUnixNano": str(base_ns + 500_000_000)},
                    {"traceId": trace_hex, "spanId": otlp_llm, "parentSpanId": otlp_root,
                     "name": "otlp-chat", "kind": 3,
                     "startTimeUnixNano": str(base_ns + 10_000_000),
                     "endTimeUnixNano": str(base_ns + 400_000_000),
                     "attributes": [
                         {"key": "gen_ai.request.model", "value": {"stringValue": "gpt-4o"}},
                         {"key": "gen_ai.usage.input_tokens", "value": {"intValue": "120"}},
                         {"key": "gen_ai.usage.output_tokens", "value": {"intValue": "30"}},
                         {"key": "input", "value": {"stringValue": "otlp question"}},
                         {"key": "output", "value": {"stringValue": "otlp answer"}}]}]}]}]}).encode()

    st, _ = post_raw("/v1/traces", otlp_payload, "application/json", {"project-name": "smoke-otlp"})
    check("otlp ingest (json)", st == 200, f"status={st}")

    st, projects2 = call("GET", "/api/v1/projects")
    otlp_proj = next((p for p in projects2 if p.get("name") == "smoke-otlp"), None) if isinstance(projects2, list) else None
    check("otlp auto-creates the project from project-name", bool(otlp_proj), f"projects={len(projects2) if isinstance(projects2, list) else '?'}")

    if otlp_proj:
        st, otlpt = call("GET", f"/api/v1/projects/{otlp_proj['id']}/traces")
        tl = otlpt.get("traces", [])
        ok = (st == 200 and otlpt.get("total") == 1 and tl and tl[0].get("trace_id") == trace_hex
              and tl[0].get("model") == "gpt-4o" and tl[0].get("span_count") == 2
              and tl[0].get("cost_usd", 0) > 0)
        check("otlp trace is queryable with mapped model/tokens/cost", ok,
              f"total={otlpt.get('total')} model={tl[0].get('model') if tl else None} cost={tl[0].get('cost_usd') if tl else None} "
              f"spans={tl[0].get('span_count') if tl else None} trace_id={tl[0].get('trace_id') if tl else None} want={trace_hex}")

    # a retried batch must not duplicate spans
    st, _ = post_raw("/v1/traces", otlp_payload, "application/json", {"project-name": "smoke-otlp"})
    if otlp_proj:
        st, otlpt = call("GET", f"/api/v1/projects/{otlp_proj['id']}/traces")
        check("otlp ingest is idempotent", otlpt.get("total") == 1, f"total={otlpt.get('total')}")

    # empty export: valid OTLP, no project created
    st, _ = post_raw("/v1/traces", b'{"resourceSpans":[]}', "application/json",
                     {"project-name": "smoke-ghost"})
    st2, projects3 = call("GET", "/api/v1/projects")
    ghost = any(p.get("name") == "smoke-ghost" for p in projects3) if isinstance(projects3, list) else True
    check("otlp empty batch answers 200 without creating a project", st == 200 and not ghost,
          f"status={st} project_created={ghost}")

    st, fb = call("POST", f"/api/v1/projects/{pid}/traces/{root}/feedback",
                  {"kind": "thumbs", "value": "up", "comment": "smoke"})
    check("feedback create", st == 200 and "id" in fb, f"status={st}")

    st, dash = call("GET", f"/api/v1/projects/{pid}/dashboard")
    ok = (st == 200 and dash.get("requests") == 1 and "latency" in dash
          and "timeseries" in dash and "model_breakdown" in dash)
    check("dashboard aggregates", ok,
          f"requests={dash.get('requests')} p95={dash.get('latency', {}).get('p95')} "
          f"models={len(dash.get('model_breakdown', []))}")

    st, scorers = call("GET", "/api/v1/scorers")
    names = {s["name"] for s in scorers} if isinstance(scorers, list) else set()
    check("scorer registry", st == 200 and {"exact_match", "factuality_judge"} <= names, f"n={len(names)}")

    # ---- datasets ----
    st, ds = call("POST", f"/api/v1/projects/{pid}/datasets",
                  {"name": "smoke dataset", "description": "smoke"})
    dsid = ds.get("id")
    check("dataset create", st == 200 and bool(dsid), f"status={st}")

    st, one_rec = call("POST", f"/api/v1/datasets/{dsid}/records",
                       {"input": "how do I reset my password?",
                        "expected": "Click 'Forgot password' and follow the email link."})
    check("dataset record add (single)", st == 200 and "id" in one_rec, f"status={st}")

    st, batch = call("POST", f"/api/v1/datasets/{dsid}/records", [
        {"input": "where is my refund?", "expected": "Refunds post within 5-7 business days."},
        {"input": "how do I cancel?", "expected": "Cancel anytime in Settings > Billing."},
    ])
    check("dataset record add (batch)", st == 200 and len(batch.get("ids", [])) == 2,
          f"status={st} ids={len(batch.get('ids', []))}")

    st, detail = call("GET", f"/api/v1/datasets/{dsid}")
    recs = detail.get("records", [])
    check("dataset detail", st == 200 and len(recs) == 3 and detail.get("version") == 3,
          f"records={len(recs)} version={detail.get('version')}")

    st, promo = call("POST", f"/api/v1/datasets/{dsid}/records:from_trace",
                     {"trace_id": root, "expected": "Click 'Forgot password'."})
    check("promote trace into dataset", st == 200 and bool(promo.get("id")), f"status={st}")
    st, detail = call("GET", f"/api/v1/datasets/{dsid}")
    promoted = [r for r in detail.get("records", []) if r.get("source") == "review"]
    check("promoted record keeps origin trace",
          bool(promoted) and (promoted[0].get("metadata") or {}).get("origin_trace_id") == root,
          f"promoted={len(promoted)}")

    # ---- prompts ----
    st, pr = call("POST", f"/api/v1/projects/{pid}/prompts",
                  {"name": "smoke-prompt", "model": "gpt-4o-mini",
                   "template": {"messages": [{"role": "user", "content": "{{input}}"}]},
                   "params": {"temperature": 0}})
    check("prompt create", st == 200 and bool(pr.get("id")), f"status={st}")
    st, pr2 = call("POST", f"/api/v1/projects/{pid}/prompts",
                   {"name": "smoke-prompt", "model": "gpt-4o-mini",
                    "template": {"messages": [{"role": "user", "content": "{{input}}"}]}})
    st, prompts = call("GET", f"/api/v1/projects/{pid}/prompts")
    versions = [p["version"] for p in prompts if p["name"] == "smoke-prompt"] if st == 200 else []
    check("prompt versioning", sorted(versions) == [1, 2], f"versions={sorted(versions)}")

    # ---- evaluation: Go dispatches to the Python worker ----
    st, exp = call("POST", f"/api/v1/projects/{pid}/evaluations",
                   {"dataset_id": dsid, "experiment_name": "smoke run",
                    "scorer_names": ["exact_match", "contains", "factuality_judge"]})
    expid = exp.get("experiment_id")
    check("eval dispatch (Go->Python)", st == 200 and bool(expid), f"status={st} exp={expid}")

    final, deadline = None, time.time() + 180
    while time.time() < deadline:
        st, ed = call("GET", f"/api/v1/experiments/{expid}")
        if ed.get("status") in ("completed", "failed", "error"):
            final = ed
            break
        time.sleep(1)
    check("eval completed", bool(final) and final.get("status") == "completed",
          f"status={final.get('status') if final else 'timeout'}")

    if final:
        runs = final.get("runs", [])
        scored = [r for r in runs if r.get("scores")]
        check("eval runs scored", len(runs) == 4 and len(scored) == 4,
              f"runs={len(runs)} scored={len(scored)}")
        check("eval runs carry score provenance",
              all(s.get("judge_mode") for r in runs for s in r["scores"].values()),
              "judge_mode on every score")
        summary = final.get("summary") or {}
        check("eval summary", bool(summary.get("scores")) and summary.get("runs") == 4,
              json.dumps(summary.get("scores"))[:160])

    st, exps = call("GET", f"/api/v1/projects/{pid}/experiments")
    check("experiments list", st == 200 and len(exps) == 1 and exps[0]["run_count"] == 4,
          f"n={len(exps)}")

    st, cmp_rows = call("GET", f"/api/v1/experiments/{expid}/compare?baseline_id={expid}")
    check("compare endpoint", st == 200 and len(cmp_rows) == 4, f"rows={len(cmp_rows)}")

    failed = [r for r in results if not r[1]]
    print(f"\n{len(results) - len(failed)}/{len(results)} checks passed")
    for name, _, detail in failed:
        print(f"  FAILED: {name} -- {detail}")
    return 1 if failed else 0


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default=None,
                    help="test an already-running server instead of spawning one")
    args = ap.parse_args()
    sys.exit(main(args.base))
