"""MCP (Model Context Protocol) server test.

Spawns a private API server on a temp database, seeds one trace and one dataset,
then drives `evalforge --mcp` over stdio exactly like Claude Code / Cursor
would: initialize → tools/list → tools/call.

Asserts the protocol handshake, that reads reflect the API, that writes land,
that an evaluation launched from MCP completes, and that bad input is reported
as a tool error rather than a crash.

Usage:  python scripts/mcp_test.py
Exit code 0 = every check passed.
"""
import json
import os
import queue
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


def api(base, method, path, payload=None, timeout=30):
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


def start_server():
    data_dir = tempfile.mkdtemp(prefix="aieval-mcp-")
    port = free_port()
    env = dict(os.environ)
    env.update({"DATA_DIR": data_dir, "ADDR": f":{port}", "OPENROUTER_API_KEY": ""})
    log = open(os.path.join(data_dir, "server.log"), "w", encoding="utf-8", errors="replace")
    proc = subprocess.Popen([server_binary()], cwd=os.path.join(ROOT, "server-go"), env=env,
                            stdout=log, stderr=subprocess.STDOUT, text=True)
    base = f"http://localhost:{port}"
    deadline = time.time() + 30
    while time.time() < deadline:
        if proc.poll() is not None:
            log.close()
            raise SystemExit("server exited during startup")
        try:
            with urllib.request.urlopen(base + "/health", timeout=2) as r:
                if json.loads(r.read().decode()).get("ok"):
                    return proc, base, log
        except Exception:
            time.sleep(0.3)
    proc.kill()
    raise SystemExit("server did not become healthy in time")


class MCPClient:
    """Minimal JSON-RPC 2.0 over stdio client, the way an MCP host does it."""

    def __init__(self, base):
        env = dict(os.environ)
        env["MCP_API_BASE"] = base
        env["OPENROUTER_API_KEY"] = ""
        self.stderr = []
        self.proc = subprocess.Popen(
            [server_binary(), "--mcp"], cwd=os.path.join(ROOT, "server-go"), env=env,
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, bufsize=1,
        )
        self.lines = queue.Queue()
        threading.Thread(target=self._pump, daemon=True).start()
        threading.Thread(target=self._pump_err, daemon=True).start()
        self.next_id = 0

    def _pump(self):
        for line in self.proc.stdout:
            self.lines.put(line)
        self.lines.put(None)

    def _pump_err(self):
        for line in self.proc.stderr:
            self.stderr.append(line.rstrip())

    def _send(self, msg):
        self.proc.stdin.write(json.dumps(msg) + "\n")
        self.proc.stdin.flush()

    def _read(self, timeout=60):
        try:
            line = self.lines.get(timeout=timeout)
        except queue.Empty:
            raise TimeoutError("no MCP response")
        if line is None:
            raise RuntimeError("MCP server closed stdout; stderr=" + "\n".join(self.stderr[-5:]))
        return json.loads(line)  # raises if stdout is not pure JSON

    def request(self, method, params=None):
        self.next_id += 1
        msg = {"jsonrpc": "2.0", "id": self.next_id, "method": method}
        if params is not None:
            msg["params"] = params
        self._send(msg)
        return self._read()

    def notify(self, method, params=None):
        msg = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            msg["params"] = params
        self._send(msg)

    def call_tool(self, name, arguments=None):
        return self.request("tools/call", {"name": name, "arguments": arguments or {}})

    def close(self):
        try:
            self.proc.stdin.close()
            self.proc.terminate()
            self.proc.wait(timeout=5)
        except Exception:
            self.proc.kill()


def tool_json(resp):
    """tools/call result -> parsed JSON payload (the platform's own JSON)."""
    result = resp.get("result") or {}
    content = result.get("content") or []
    if not content:
        return None
    return json.loads(content[0]["text"])


def main():
    server, base, log = start_server()
    client = None
    try:
        # ---- seed real data through the public API ----
        st, proj = api(base, "POST", "/api/v1/projects",
                       {"name": "mcp-test", "description": "seeded for the MCP test"})
        pid = proj["id"]
        now = int(time.time() * 1000)
        trace_id = "mcp-trace-1"
        api(base, "POST", f"/api/v1/projects/{pid}/spans:ingest", {"events": [
            {"id": trace_id, "name": "support_agent", "type": "task",
             "input": "how do I reset my password?", "start_ms": now},
            {"id": "mcp-tool", "parent_id": trace_id, "name": "retrieve", "type": "tool",
             "input": {"q": "reset"}, "output": {"doc": "reset_password"},
             "start_ms": now + 5, "end_ms": now + 20},
            {"id": "mcp-llm", "parent_id": trace_id, "name": "answer", "type": "llm",
             "model": "gpt-4o-mini", "input": "q", "output": "Use Forgot password.",
             "start_ms": now + 20, "end_ms": now + 120},
        ]})
        st, ds = api(base, "POST", f"/api/v1/projects/{pid}/datasets",
                     {"name": "mcp-cases", "description": "mcp"})
        dsid = ds["id"]
        api(base, "POST", f"/api/v1/datasets/{dsid}/records", [
            {"input": "how do I reset my password?", "expected": "Use Forgot password."},
            {"input": "where is my refund?", "expected": "Refunds post in 5-7 days."},
        ])

        # ---- protocol ----
        client = MCPClient(base)
        init = client.request("initialize", {
            "protocolVersion": "2025-06-18",
            "capabilities": {},
            "clientInfo": {"name": "mcp-test", "version": "0.1"},
        })
        r = init.get("result") or {}
        check("initialize negotiates a protocol version",
              r.get("protocolVersion") == "2025-06-18", f"got {r.get('protocolVersion')}")
        check("initialize advertises the tools capability",
              "tools" in (r.get("capabilities") or {}), json.dumps(r.get("capabilities")))
        check("initialize identifies the server",
              (r.get("serverInfo") or {}).get("name") == "evalforge",
              str(r.get("serverInfo")))
        client.notify("notifications/initialized")

        ping = client.request("ping")
        check("ping answers", "result" in ping)

        listed = client.request("tools/list")
        tools = {t["name"]: t for t in (listed.get("result") or {}).get("tools", [])}
        expected = {"list_projects", "search_traces", "get_trace", "get_dashboard",
                    "list_datasets", "get_dataset", "list_experiments", "get_experiment",
                    "compare_experiments", "add_feedback", "run_evaluation"}
        check("tools/list exposes the platform surface",
              expected <= set(tools), f"n={len(tools)} missing={sorted(expected - set(tools))}")
        check("every tool carries a JSON schema",
              all((t.get("inputSchema") or {}).get("type") == "object" for t in tools.values()))

        # ---- reads ----
        projects = tool_json(client.call_tool("list_projects"))
        check("list_projects returns the seeded project",
              any(p.get("name") == "mcp-test" for p in projects), f"n={len(projects)}")

        traces = tool_json(client.call_tool("search_traces", {"project_name": "mcp-test"}))
        t = (traces.get("traces") or [{}])[0]
        check("search_traces resolves by project_name and maps cost/model",
              traces.get("total") == 1 and t.get("model") == "gpt-4o-mini" and t.get("cost_usd", 0) > 0,
              f"total={traces.get('total')} model={t.get('model')} cost={t.get('cost_usd')}")

        trace = tool_json(client.call_tool("get_trace", {"project_id": pid, "trace_id": trace_id}))
        kids = (trace.get("span_tree") or [{}])[0].get("children", [])
        check("get_trace returns the nested span tree", len(kids) == 2, f"children={len(kids)}")

        dash = tool_json(client.call_tool("get_dashboard", {"project_id": pid}))
        check("get_dashboard aggregates", dash.get("requests", 0) >= 1,
              f"requests={dash.get('requests')}")

        datasets = tool_json(client.call_tool("list_datasets", {"project_name": "mcp-test"}))
        dsrow = next((d for d in datasets if d.get("id") == dsid), None)
        check("list_datasets finds the dataset", bool(dsrow))

        detail = tool_json(client.call_tool("get_dataset", {"dataset_id": dsid}))
        check("get_dataset returns records", len(detail.get("records", [])) == 2,
              f"records={len(detail.get('records', []))}")

        # ---- write: feedback ----
        fb = tool_json(client.call_tool("add_feedback", {
            "project_id": pid, "trace_id": trace_id, "kind": "thumbs",
            "value": "up", "comment": "written through MCP",
        }))
        check("add_feedback writes through the API", bool(fb.get("id")), str(fb))
        after = tool_json(client.call_tool("get_trace", {"project_id": pid, "trace_id": trace_id}))
        check("feedback is visible to a subsequent read",
              any(f.get("comment") == "written through MCP" for f in after.get("feedback", [])),
              f"feedback={len(after.get('feedback', []))}")

        # ---- write: evaluation, launched and polled entirely over MCP ----
        started = tool_json(client.call_tool("run_evaluation", {
            "project_id": pid, "dataset_id": dsid, "name": "mcp run",
            "scorers": ["exact_match", "contains"],
        }))
        exp_id = started.get("experiment_id")
        check("run_evaluation returns an experiment id", bool(exp_id), str(started))

        final = None
        deadline = time.time() + 90
        while time.time() < deadline:
            cur = tool_json(client.call_tool("get_experiment", {"experiment_id": exp_id}))
            if cur.get("status") in ("completed", "failed"):
                final = cur
                break
            time.sleep(1.5)
        check("experiment reaches a terminal state",
              bool(final) and final.get("status") == "completed",
              f"status={final.get('status') if final else 'timeout'}")
        if final:
            runs = final.get("runs", [])
            check("evaluation runs are scored", len(runs) == 2 and all(r.get("scores") for r in runs),
                  f"runs={len(runs)}")

        # second experiment + compare, the actual "did it help?" question
        started2 = tool_json(client.call_tool("run_evaluation", {
            "project_id": pid, "dataset_id": dsid, "name": "mcp run 2",
            "scorers": ["exact_match"], "baseline_experiment_id": exp_id,
        }))
        exp2 = started2.get("experiment_id")
        deadline = time.time() + 90
        done2 = False
        while time.time() < deadline:
            cur = tool_json(client.call_tool("get_experiment", {"experiment_id": exp2}))
            if cur.get("status") in ("completed", "failed"):
                done2 = cur.get("status") == "completed"
                break
            time.sleep(1.5)
        comparison = tool_json(client.call_tool("compare_experiments", {
            "experiment_id": exp2, "baseline_id": exp_id,
        })) if done2 else []
        check("compare_experiments aligns cases against the baseline",
              done2 and isinstance(comparison, list) and len(comparison) == 2,
              f"rows={len(comparison) if isinstance(comparison, list) else comparison}")

        # ---- error handling ----
        # Per the MCP spec, an unknown tool is a *protocol* error (JSON-RPC),
        # while a tool that runs and fails returns result.isError = true.
        unknown = client.call_tool("nope", {})
        check("unknown tool is a protocol error (-32602)",
              (unknown.get("error") or {}).get("code") == -32602, str(unknown)[:120])
        check("server still answers after a rejected tool call",
              "result" in client.request("ping"))

        missing = client.call_tool("get_trace", {"project_id": pid})
        check("missing required argument is reported",
              (missing.get("result") or {}).get("isError") is True, str(missing)[:120])

        bad = client.request("no/such/method")
        check("unknown method returns JSON-RPC -32601",
              (bad.get("error") or {}).get("code") == -32601, str(bad)[:120])

        check("stdout carried only JSON-RPC messages", client.lines.qsize() == 0,
              f"queued={client.lines.qsize()}")
    finally:
        if client:
            client.close()
        server.terminate()
        try:
            server.wait(timeout=10)
        except Exception:
            server.kill()
        log.close()

    failed = [r for r in results if not r[1]]
    print(f"\n{len(results) - len(failed)}/{len(results)} checks passed")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
