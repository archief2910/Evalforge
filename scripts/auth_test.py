#!/usr/bin/env python3
"""End-to-end auth check for EvalForge.

Starts a private server with AUTH_MODE=required on a throw-away database and
drives the exact flow a remote MCP client performs, plus the failure modes that
matter for safety:

  discovery -> dynamic registration -> authorize (form, PKCE S256) -> token
  -> protected MCP call -> refresh rotation -> replay rejection -> revocation

It also proves scope enforcement, project isolation, origin validation and that
a revoked key kills the tokens derived from it.

Run:  python scripts/auth_test.py  [--base URL]   (--base tests an existing server)
"""

import argparse
import base64
import hashlib
import json
import os
import secrets
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
REDIRECT = "http://127.0.0.1:7777/callback"

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


def request(method, path, base, token=None, body=None, form=None, headers=None, retry=None):
    """Returns (status, parsed-or-text body, headers). Never raises on 4xx/5xx."""
    url = base + path
    data = None
    hdrs = dict(headers or {})
    if token:
        hdrs["Authorization"] = "Bearer " + token
    if form is not None:
        data = urllib.parse.urlencode(form).encode()
        hdrs["Content-Type"] = "application/x-www-form-urlencoded"
    elif body is not None:
        data = json.dumps(body).encode()
        hdrs["Content-Type"] = "application/json"
    opener = urllib.request.build_opener(NoRedirect)
    # Windows occasionally resets a fresh loopback connection before the status
    # line arrives. Retrying is safe here only where replaying cannot change
    # state: GETs and unauthenticated requests (which are rejected by the auth
    # layer anyway). Mutating authenticated calls are never retried, because a
    # retried tool call could apply twice.
    if retry is None:
        retry = method == "GET" or token is None
    attempts = 2 if retry else 1
    for attempt in range(attempts):
        req = urllib.request.Request(url, data=data, headers=hdrs, method=method)
        try:
            with opener.open(req, timeout=30) as r:
                raw = r.read().decode()
                return r.status, parse(raw), lower(r.headers)
        except urllib.error.HTTPError as e:
            raw = e.read().decode()
            return e.code, parse(raw), lower(e.headers)
        except (ConnectionResetError, urllib.error.URLError) as e:
            if attempt + 1 == attempts:
                raise SystemExit(f"transport failed for {method} {path}: {e}")
            time.sleep(0.2)
    raise AssertionError("unreachable")


def lower(headers):
    """Case-insensitive header lookup: urllib preserves Go's canonical casing."""
    return {k.lower(): v for k, v in headers.items()}


def parse(raw):
    try:
        return json.loads(raw)
    except Exception:
        return raw


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None


def pkce():
    verifier = secrets.token_urlsafe(48)[:64]
    digest = hashlib.sha256(verifier.encode()).digest()
    challenge = base64.urlsafe_b64encode(digest).decode().rstrip("=")
    return verifier, challenge


def base_url(port):
    return f"http://localhost:{port}"


def start_server():
    data_dir = tempfile.mkdtemp(prefix="evalforge-auth-")
    port = free_port()
    env = dict(os.environ)
    env["DATA_DIR"] = data_dir
    env["ADDR"] = f":{port}"
    env["AUTH_MODE"] = "required"
    # Pin the issuer to the origin this test actually talks to; without it the
    # issuer defaults to 127.0.0.1 while the client uses localhost.
    env["PUBLIC_BASE_URL"] = base_url(port)
    env["OPENROUTER_API_KEY"] = ""
    log_path = os.path.join(data_dir, "server.log")
    log_file = open(log_path, "w", encoding="utf-8", errors="replace")
    proc = subprocess.Popen([server_binary()], cwd=os.path.join(ROOT, "server-go"), env=env,
                            stdout=log_file, stderr=subprocess.STDOUT, text=True)
    base = base_url(port)
    deadline = time.time() + 30
    while time.time() < deadline:
        if proc.poll() is not None:
            log_file.close()
            with open(log_path, encoding="utf-8", errors="replace") as f:
                raise SystemExit("server exited during startup:\n" + f.read())
        try:
            with urllib.request.urlopen(base + "/health", timeout=2) as r:
                if json.loads(r.read().decode()).get("ok"):
                    return proc, base, data_dir
        except Exception:
            time.sleep(0.3)
    proc.kill()
    raise SystemExit("server did not become healthy in time")


def stop_server(proc):
    try:
        proc.terminate()
        proc.wait(timeout=10)
    except Exception:
        proc.kill()


def mint_key(base, name, scopes, project_id=None, token=None):
    body = {"name": name, "scopes": scopes, "owner": name + "@example.com"}
    if project_id:
        body["project_id"] = project_id
    status, out, _ = request("POST", "/api/v1/keys", base, token=token, body=body)
    return status, out


def issue_token(base, admin, client_id, scope="read write"):
    verifier, challenge = pkce()
    form = {
        "response_type": "code", "client_id": client_id, "redirect_uri": REDIRECT,
        "scope": scope, "state": "st-" + secrets.token_hex(4),
        "code_challenge": challenge, "code_challenge_method": "S256",
        "decision": "allow", "api_key": admin,
    }
    status, _, headers = request("POST", "/oauth/authorize", base, form=form)
    location = headers.get("location", "")
    code = urllib.parse.parse_qs(urllib.parse.urlparse(location).query).get("code", [""])[0]
    status2, out, _ = request("POST", "/oauth/token", base, form={
        "grant_type": "authorization_code", "code": code, "client_id": client_id,
        "redirect_uri": REDIRECT, "code_verifier": verifier,
    })
    return status, status2, out, code, verifier, location


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", help="test an already-running server instead of booting one")
    args = ap.parse_args()

    proc = None
    data_dir = None
    if args.base:
        base = args.base.rstrip("/")
    else:
        proc, base, data_dir = start_server()
    try:
        run(base)
    finally:
        if proc:
            stop_server(proc)
            print(f"\nserver log: {os.path.join(data_dir, 'server.log')}")

    passed = sum(1 for _, ok, _ in results if ok)
    print(f"\n{passed}/{len(results)} checks passed")
    failed = [n for n, ok, _ in results if not ok]
    if failed:
        print("failed: " + ", ".join(failed))
    return 0 if passed == len(results) else 1


def run(base):
    # ---- public surface + the challenge an MCP client follows ----
    status, out, _ = request("GET", "/health", base)
    check("health is public", status == 200 and out.get("ok") is True, str(status))

    status, out, headers = request("GET", "/api/v1/projects", base)
    challenge = headers.get("www-authenticate", "")
    check("unauthenticated API answers 401", status == 401, str(status))
    check("401 carries resource_metadata", "resource_metadata=" in challenge
          and "oauth-protected-resource" in challenge, challenge[:80])

    status, meta, _ = request("GET", "/.well-known/oauth-authorization-server", base)
    check("authorization-server metadata", status == 200 and meta.get("issuer") == base
          and meta.get("code_challenge_methods_supported") == ["S256"], str(meta.get("issuer")))

    status, prm, _ = request("GET", "/.well-known/oauth-protected-resource", base)
    check("protected-resource metadata", status == 200 and prm.get("resource") == base + "/mcp",
          str(prm.get("resource")))

    # ---- bootstrap + keys ----
    status, out = mint_key(base, "bootstrap", ["read", "write", "admin"])
    admin = out.get("key", "")
    check("first key can be minted (bootstrap)", status == 201 and admin.startswith("efk_"), str(status))
    check("key is returned once with a hash-only note",
          "key" in out and "note" in out and out.get("scopes") == ["read", "write", "admin"], str(out.get("prefix")))

    status, who, _ = request("GET", "/api/v1/whoami", base, token=admin)
    check("whoami with API key", status == 200 and who.get("kind") == "api_key" and who.get("authenticated") is True,
          str(who.get("scopes")))

    status, listing, _ = request("GET", "/api/v1/keys", base, token=admin)
    check("key listing never contains a secret",
          status == 200 and admin not in json.dumps(listing), f"{len(listing)} keys")

    status, out = mint_key(base, "viewer", ["read"], token=admin)
    read_key = out.get("key", "")
    check("read-only key minted", status == 201, str(status))
    status, _, _ = request("GET", "/api/v1/projects", base, token=read_key)
    check("read-only key can read", status == 200, str(status))
    status, body, _ = request("POST", "/api/v1/projects", base, token=read_key, body={"name": "nope"})
    check("read-only key cannot write", status == 403, str(status))
    status, _, _ = request("GET", "/api/v1/keys", base, token=read_key)
    check("read-only key cannot manage keys", status == 403, str(status))

    status, _, _ = request("GET", "/api/v1/projects", base, token="efk_invalid")
    check("unknown key rejected", status == 401, str(status))

    # ---- dynamic client registration ----
    status, reg, _ = request("POST", "/oauth/register", base, body={
        "client_name": "Claude Code", "redirect_uris": [REDIRECT],
        "token_endpoint_auth_method": "none",
    })
    client_id = reg.get("client_id", "")
    check("dynamic client registration", status == 201 and client_id.startswith("efc_"),
          f"{client_id} {reg.get('token_endpoint_auth_method')}")

    status, _, _ = request("POST", "/oauth/register", base, body={"redirect_uris": ["http://evil.example.com/cb"]})
    check("registration rejects non-loopback http redirect", status == 400, str(status))

    status, _, _ = request("POST", "/oauth/register", base, body={
        "redirect_uris": [REDIRECT], "grant_types": ["password"]})
    check("registration rejects password grant", status == 400, str(status))

    # ---- authorize: rendering, refusal, then a real code ----
    verifier, challenge = pkce()
    params = urllib.parse.urlencode({
        "response_type": "code", "client_id": client_id, "redirect_uri": REDIRECT,
        "scope": "read write", "state": "state-1",
        "code_challenge": challenge, "code_challenge_method": "S256",
    })
    status, page, _ = request("GET", "/oauth/authorize?" + params, base)
    check("consent page names the client and scopes",
          status == 200 and "Claude Code" in page and "read" in page, str(status))

    form = urllib.parse.parse_qs(params)
    form = {k: v[0] for k, v in form.items()}
    form.update({"decision": "allow", "api_key": "efk_wrong"})
    status, _, headers = request("POST", "/oauth/authorize", base, form=form)
    check("wrong API key cannot authorize", status == 401 and "Location" not in headers, str(status))

    form["api_key"] = admin
    form["state"] = "state-1"
    status, _, headers = request("POST", "/oauth/authorize", base, form=form)
    location = headers.get("location", "")
    q = urllib.parse.parse_qs(urllib.parse.urlparse(location).query)
    code = q.get("code", [""])[0]
    check("authorize redirects with code and state", status == 302 and code.startswith("efc_")
          and q.get("state") == ["state-1"], location[:60])

    # ---- token exchange ----
    bad = {"grant_type": "authorization_code", "code": code, "client_id": client_id,
           "redirect_uri": REDIRECT, "code_verifier": "x" * 50}
    status, body, _ = request("POST", "/oauth/token", base, form=bad)
    check("PKCE mismatch is rejected", status == 400 and body.get("error") == "invalid_grant",
          str(body.get("error_description"))[:60])

    good = dict(bad, code_verifier=verifier)
    status, tokens, _ = request("POST", "/oauth/token", base, form=good)
    access = tokens.get("access_token", "")
    refresh = tokens.get("refresh_token", "")
    check("code + PKCE exchanges for tokens", status == 200 and access.startswith("efa_")
          and refresh.startswith("efr_") and tokens.get("token_type") == "Bearer", str(status))

    status, who, _ = request("GET", "/api/v1/whoami", base, token=access)
    check("access token authenticates as OAuth principal",
          status == 200 and who.get("kind") == "oauth" and who.get("client_id") == client_id, str(status))

    # ---- the MCP endpoint accepts that token ----
    status, init, _ = request("POST", "/mcp", base, token=access, body={
        "jsonrpc": "2.0", "id": 1, "method": "initialize",
        "params": {"protocolVersion": "2025-06-18", "capabilities": {}},
    }, retry=True)
    info = (init.get("result") or {}).get("serverInfo", {}) if isinstance(init, dict) else {}
    check("MCP initialize over HTTP with bearer",
          status == 200 and info.get("name") == "evalforge", str(info))

    status, listing, _ = request("POST", "/mcp", base, token=access, body={
        "jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}}, retry=True)
    tools = [t["name"] for t in (listing.get("result") or {}).get("tools", [])] if isinstance(listing, dict) else []
    check("MCP exposes 11 tools", status == 200 and len(tools) == 11, ",".join(sorted(tools)[:4]) + "…")

    status, _, _ = request("POST", "/mcp", base, body={
        "jsonrpc": "2.0", "id": 3, "method": "ping"})
    check("MCP without a token is 401", status == 401, str(status))

    status, _, _ = request("POST", "/mcp", base, token=access, body={
        "jsonrpc": "2.0", "id": 4, "method": "ping"}, headers={"Origin": "https://evil.example.com"}, retry=True)
    check("MCP rejects a foreign Origin", status == 403, str(status))

    # ---- project isolation ----
    _, p1, _ = request("POST", "/api/v1/projects", base, token=admin, body={"name": "alpha"})
    _, p2, _ = request("POST", "/api/v1/projects", base, token=admin, body={"name": "beta"})
    status, scoped = mint_key(base, "alpha-only", ["read"], project_id=p1["id"], token=admin)
    scoped_key = scoped.get("key", "")
    check("project-scoped key minted", status == 201 and scoped.get("project_id") == p1["id"], str(status))
    status, _, _ = request("GET", f"/api/v1/projects/{p1['id']}", base, token=scoped_key)
    check("scoped key reads its own project", status == 200, str(status))
    status, _, _ = request("GET", f"/api/v1/projects/{p2['id']}", base, token=scoped_key)
    check("scoped key cannot read another project", status == 404, str(status))
    status, listed, _ = request("GET", "/api/v1/projects", base, token=scoped_key)
    check("project list is scoped to the credential",
          status == 200 and isinstance(listed, list) and len(listed) == 1 and listed[0]["id"] == p1["id"],
          f"{len(listed) if isinstance(listed, list) else listed} project(s)")

    # ---- refresh rotation + reuse detection ----
    status, rotated, _ = request("POST", "/oauth/token", base, form={
        "grant_type": "refresh_token", "refresh_token": refresh, "client_id": client_id})
    access2 = rotated.get("access_token", "")
    check("refresh rotates the token pair", status == 200 and access2 and access2 != access, str(status))
    status, _, _ = request("GET", "/api/v1/whoami", base, token=access2)
    check("rotated access token works", status == 200, str(status))
    status, body, _ = request("POST", "/oauth/token", base, form={
        "grant_type": "refresh_token", "refresh_token": refresh, "client_id": client_id})
    check("reused refresh token is refused", status == 400 and "revoked" in str(body.get("error_description", "")),
          str(body.get("error_description"))[:60])
    status, _, _ = request("GET", "/api/v1/whoami", base, token=access2)
    check("reuse revokes the whole token family", status == 401, str(status))

    # ---- revocation and key -> token cascade ----
    _, tok_status, fresh, _, _, _ = issue_token(base, admin, client_id)
    fresh_access = fresh.get("access_token", "")
    check("secondary credential issues tokens", tok_status == 200 and bool(fresh_access), str(tok_status))
    status, _, _ = request("POST", "/oauth/revoke", base, form={"token": fresh_access})
    check("revocation endpoint accepts the token", status == 200, str(status))
    status, _, _ = request("GET", "/api/v1/whoami", base, token=fresh_access)
    check("revoked token stops working", status == 401, str(status))

    status, temp = mint_key(base, "temp", ["read", "write"], token=admin)
    temp_key, temp_id = temp.get("key", ""), temp.get("id", "")
    # A token issued from the temp key must die when the key is revoked.
    verifier3, challenge3 = pkce()
    form3 = {"response_type": "code", "client_id": client_id, "redirect_uri": REDIRECT,
             "scope": "read", "code_challenge": challenge3, "code_challenge_method": "S256",
             "decision": "allow", "api_key": temp_key}
    _, _, headers3 = request("POST", "/oauth/authorize", base, form=form3)
    code3 = urllib.parse.parse_qs(urllib.parse.urlparse(headers3.get("location", "")).query).get("code", [""])[0]
    _, t3, _ = request("POST", "/oauth/token", base, form={
        "grant_type": "authorization_code", "code": code3, "client_id": client_id,
        "redirect_uri": REDIRECT, "code_verifier": verifier3})
    temp_access = t3.get("access_token", "")
    check("token issued from a secondary key", bool(temp_access), str(list(t3)[:3]))
    status, _, _ = request("DELETE", "/api/v1/keys/" + temp_id, base, token=admin)
    check("admin revokes the key", status == 200, str(status))
    status, _, _ = request("GET", "/api/v1/whoami", base, token=temp_access)
    check("revoking a key kills tokens derived from it", status == 401, str(status))
    status, _, _ = request("GET", "/api/v1/whoami", base, token=temp_key)
    check("revoked key rejected", status == 401, str(status))

    # ---- MCP scope enforcement with a read-only credential ----
    status, out, _ = request("POST", "/mcp", base, token=read_key, body={
        "jsonrpc": "2.0", "id": 9, "method": "tools/call",
        "params": {"name": "add_feedback", "arguments": {
            "project_id": p1["id"], "trace_id": "t", "kind": "thumbs", "value": "up"}},
    })
    result = (out.get("result") or {}) if isinstance(out, dict) else {}
    text = (result.get("content") or [{}])[0].get("text", "")
    check("read-only credential cannot run a write tool",
          status == 200 and result.get("isError") is True and "insufficient scope" in text, text[:70])

    status, out, _ = request("POST", "/mcp", base, token=read_key, body={
        "jsonrpc": "2.0", "id": 10, "method": "tools/call",
        "params": {"name": "list_datasets", "arguments": {"project_id": p1["id"]}},
    })
    result = (out.get("result") or {}) if isinstance(out, dict) else {}
    err_text = (result.get("content") or [{}])[0].get("text", "") if result.get("isError") else ""
    check("read-only credential can run a read tool",
          status == 200 and result.get("isError") is not True, err_text[:90] or str(status))


if __name__ == "__main__":
    sys.exit(main())
