# EvalForge

An original, production-lean implementation of the LLM observability + evaluation loop:
**instrument → observe traces → review → promote to dataset → run evaluations → compare vs baseline.**

Three docs worth reading first:
- **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** — how the system is actually wired (diagrams, component/tech
  table, data model, faithfulness vs Braintrust, prioritised gap analysis).
- **[docs/OPIK_ALIGNMENT.md](docs/OPIK_ALIGNMENT.md)** — a decision-by-decision comparison with the Opik source in
  `opik/`: what we copy, what we adapt, what we reject, plus the corrected Go layout and the P0/P1/P2 roadmap.
- **[docs/REVERSE_ENGINEERING.md](docs/REVERSE_ENGINEERING.md)** — the research behind it: observed facts about
  Braintrust, our inferred architecture, data model, API design, MVP scope and monetization thesis.

Open [docs/architecture.html](docs/architecture.html) in a browser for the rendered diagram.

## Stack
- **API server**: Go (`server-go/`, module `evalforge`) — single static binary, owns the SQLite database and the whole
  `/api/v1` surface (ingest, traces, dashboard, datasets, prompts, experiments) plus the MCP and OAuth endpoints.
- **Eval worker**: Python (`worker-py/worker.py`) — the Go server dispatches an eval job to it; the worker
  generates outputs, applies the scorers, and posts results back over the worker-callback API.
- **Frontend**: React 19 + Vite, original dark UI (`frontend/`), proxies `/api` → `localhost:8000`.
- **SDK sample**: `sdk/sample_client.py` — proves the ingestion surface (batching, nested spans, flush).

## Run it
```bash
# 1) API server (from ai-eval-platform/server-go)
cd ai-eval-platform/server-go
go build -o evalforge.exe . && ./evalforge.exe     # → http://localhost:8000

# 2) demo data (seeds a project with 36 traces, 2 datasets, 2 prompts)
cd ai-eval-platform && python worker-py/seed.py

# 3) frontend (new terminal)
cd ai-eval-platform/frontend
npm install      # already done if node_modules exists
npm run dev      # → http://localhost:5173
```

Optional real LLM provider (higher-quality judges/tasks; everything works offline without it).
Put these in `ai-eval-platform/.env` (git-ignored, never committed):
```bash
OPENROUTER_API_KEY=sk-or-...
OPENROUTER_BASE_URL=https://openrouter.ai/api/v1
```
The Go server loads `.env` on start and passes the environment to the worker. The model id is a
fixed literal (`openrouter/free`) inside `worker-py/worker.py`, because the router rejects any other
value — it is deliberately not configurable.

The free router picks reasoning models, which can spend the whole token budget before writing an
answer. The worker handles that by escalating `max_tokens` and retrying, and records
`judge_mode: provider_error` (never a fake `0.0`) when no answer arrives.

## The core workflow (5 minutes)
1. **Logs & Traces** — filter by status/model/has-feedback, open a trace: nested span tree + timeline, per-span tokens/cost.
2. **Review** — thumbs + comment on a trace, edit the *expected* answer.
3. **Add to dataset** — one click promotes the trace (`input` copied, `expected` = your corrected answer, `metadata.origin_trace_id` = reference back to the trace).
4. **Run evaluation** — pick a dataset, select scorers (deterministic + LLM-as-judge), choose a baseline experiment → an immutable experiment is created.
5. **Compare** — per-case deltas by input alignment; improvement/regression badges per scorer.
6. **Dashboard** — requests, error rate, p50/p95/p99 latency, token/cost totals, model breakdown.

## API surface (`/api/v1`)
| Area | Endpoints |
|---|---|
| Projects | `GET/POST /projects`, `GET /projects/{id}` |
| Ingest | `POST /projects/{id}/spans:ingest` (batch of span events), `POST /v1/traces` (OTLP/HTTP, protobuf + JSON) |
| Observe | `GET /projects/{id}/traces`, `GET /projects/{id}/traces/{trace_id}`, `POST /projects/{id}/traces/{trace_id}/feedback` |
| Dashboard | `GET /projects/{id}/dashboard` |
| Datasets | `GET/POST /projects/{id}/datasets`, `GET /datasets/{id}`, `POST /datasets/{id}/records` (single object **or** array), `POST /datasets/{id}/records:from_trace` |
| Evaluate | `GET /scorers`, `POST /projects/{id}/evaluations`, `GET /projects/{id}/experiments`, `GET /experiments/{id}`, `GET /experiments/{id}/compare?baseline_id=` |
| Prompts | `GET/POST /projects/{id}/prompts` |
| Credentials | `GET/POST /keys`, `DELETE /keys/{id}`, `GET /whoami` |
| Worker | `POST /worker_callback/{experiment_id}` (results written by the Python worker) |

## Ingest from your app

**Any OpenTelemetry SDK works** — point the OTLP/HTTP exporter at `/v1/traces` and set the project
name. Traces appear with model, tokens and cost mapped from `gen_ai.*` / `llm.*` attributes.

```python
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
exporter = OTLPSpanExporter(
    endpoint="http://localhost:8000/v1/traces",
    headers={"project-name": "my-agent"},   # auto-created on first batch
)
# then attach it to a TracerProvider with BatchSpanProcessor(...)
```

Our own SDK, for structured spans and the batched fast path:

```python
import sys; sys.path.insert(0, "sdk")
from sample_client import Client
c = Client("<project-id>", api_key="efk_...")   # api_key optional unless AUTH_MODE=required
root = c.start_span("my_agent", input="...")            # nested spans
tool = c.start_span("retrieve", type="tool", input={...}); tool.end(output={...})
llm  = c.start_span("answer", type="llm", model="gpt-4o-mini", input=[...]); llm.end(output="...")
c.flush()
```

## Use it from Claude Code / Cursor (MCP)

The binary speaks **Model Context Protocol** over two transports, so an AI host can query and operate
the platform directly — the same idea as Opik's `opik-mcp`.

**Local (stdio).** The host launches the binary; it reads `MCP_API_KEY` for its credential.

```bash
claude mcp add evalforge \
  --env MCP_API_BASE=http://localhost:8000 \
  --env MCP_API_KEY=efk_... \
  -- ./server-go/evalforge.exe --mcp
```

Or in `mcpServers` JSON (Cursor / VS Code / Claude Desktop):

```json
{
  "mcpServers": {
    "evalforge": {
      "command": "C:/absolute/path/to/server-go/evalforge.exe",
      "args": ["--mcp"],
      "env": { "MCP_API_BASE": "http://localhost:8000", "MCP_API_KEY": "efk_..." }
    }
  }
}
```

**Remote (Streamable HTTP).** Point the host at `POST /mcp` with an OAuth bearer token. The server
publishes RFC 9728 protected-resource metadata, so a compliant client discovers the authorization
server from the `WWW-Authenticate` challenge and runs the flow itself: dynamic registration →
authorization code + PKCE (S256) → short-lived access token → refresh.

```bash
curl -s http://localhost:8000/.well-known/oauth-protected-resource
curl -s -X POST http://localhost:8000/mcp \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

Tools: `list_projects`, `search_traces`, `get_trace`, `get_dashboard`, `list_datasets`, `get_dataset`,
`list_experiments`, `get_experiment`, `compare_experiments`, `add_feedback`, `run_evaluation`.

Then ask things like *"which traces errored in the last 24 hours?"*, *"pull trace X and tell me what the
model got wrong"*, or *"run the regression dataset against this baseline and list the regressed cases"*.

The MCP layer is an HTTP client of the API and opens no database, so it can point at any deployment,
and every tool call carries the caller's own credential — an agent can never see more than the key or
token that authorized it.

## Auth: keys, OAuth 2.1, multi-user

Two credentials resolve to the same principal (owner + scopes + optional project):

| Credential | Lifetime | Use |
|---|---|---|
| API key `efk_…` | long (optional expiry) | humans, CI, the stdio MCP server; sent as `Authorization: Bearer` or `X-API-Key` |
| OAuth access token `efa_…` | 1 hour | remote MCP clients; issued by the code + PKCE flow, refreshable |

Scopes are `read`, `write`, `admin` (`admin` implies the others). A key can be scoped to a single
project, and then every route — including agent tool calls — answers 404 for any other project.

Enforcement is off by default for the local loop. Turn it on for any shared deployment:

```bash
AUTH_MODE=required PUBLIC_BASE_URL=https://eval.example.com ./evalforge.exe
# first key: the bootstrap window lets an empty deployment mint one over HTTP,
# or mint it locally without opening a port:
./evalforge.exe --create-key "alice" --owner alice@example.com
```

With `AUTH_MODE=required` and zero keys, `POST /api/v1/keys` is allowed once and the resulting key is
forced to `admin` — otherwise a fresh deployment would be locked out of its own credential management.

```bash
KEY=$(./evalforge.exe --create-key "ci" --owner ci@example.com)
curl -s -X POST localhost:8000/api/v1/keys -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"dashboard","owner":"viewer@example.com","scopes":["read"]}'
```

The plaintext key is returned exactly once; only its SHA-256 hash is stored. `DELETE /api/v1/keys/{id}`
revokes the key **and every OAuth token issued from it**. The access log carries `owner` for every
authenticated request, so multi-user activity is attributable.

OAuth surface (all of it, since MCP clients require it): `GET /.well-known/oauth-protected-resource`,
`GET /.well-known/oauth-authorization-server`, `POST /oauth/register` (RFC 7591 dynamic registration,
loopback-or-https redirects only), `GET/POST /oauth/authorize` (consent page, PKCE S256 **required**),
`POST /oauth/token` (code + refresh, rotation with reuse detection), `POST /oauth/revoke` (RFC 7009).
Expired codes and tokens are scrubbed hourly.

## Scripts
```bash
python scripts/smoke_api.py     # 31-check contract run on a private server + temp DB (demo data untouched)
python scripts/auth_test.py     # 44 checks: keys, scopes, PKCE flow, refresh rotation, revocation, MCP auth
python scripts/fault_test.py    # 16 checks: crash mid-eval, hung worker, worker crash + retry
python scripts/load_test.py     # concurrent ingest/query benchmark with integrity assertions
python scripts/mcp_test.py      # 23 MCP checks: handshake, tools, reads, writes, eval, errors
python worker-py/seed.py        # re-seed the demo project (no-op if it already exists)
```
`smoke_api.py` boots its own server instance on a free port with a throw-away `DATA_DIR` and no
provider key, so it covers ingest → traces → feedback → dashboard → datasets → prompts → eval →
compare deterministically without touching your demo database. Pass `--base URL` to point it at an
already-running server instead.

`auth_test.py` starts a second private server with `AUTH_MODE=required` and drives the flow a real MCP
client would: registration, authorize (form + PKCE), token exchange, a protected MCP call, refresh,
replay rejection, revocation, and cross-project isolation.

`fault_test.py` hard-kills the server mid-evaluation (`taskkill /T /F`), restarts it on the same
database and asserts the experiment finishes with no duplicated runs; it also proves a hung worker is
killed at `WORKER_TIMEOUT_SECONDS` and a crashed worker is retried exactly once. `load_test.py`
reports spans/second and p50/p95 for ingest, trace list and dashboard, and fails if any span is lost
or duplicated.

Environment knobs for the server/worker split:

| Variable | Default | Meaning |
|---|---|---|
| `DATA_DIR` | `../data` | SQLite directory |
| `ADDR` | `:8000` | listen address |
| `AUTH_MODE` | `off` | `required` enforces API keys / OAuth tokens on every non-public route |
| `PUBLIC_BASE_URL` | listen address | public origin used in OAuth discovery and token audience (set it behind a proxy) |
| `WORKER_PATH` | `../worker-py/worker.py` | worker script the server executes |
| `WORKER_TIMEOUT_SECONDS` | `720` | hard cap per evaluation; a stalled worker is marked `failed` instead of running forever |
| `LOG_LEVEL` | `info` | structured JSON logs (`debug`, `info`, `warn`, `error`) |
| `GO_SERVER` | `http://localhost:8000` | where the worker calls back to |
| `MCP_API_BASE` | `http://localhost:8000` | API the stdio MCP server talks to |
| `MCP_API_KEY` | – | credential the stdio MCP server sends |

## Product notes (why this loops)
- Traces, datasets and experiments share one span-shaped data model, so production failures become regression tests without re-plumbing.
- Honest scoring: every score carries its `judge_mode` (`llm` vs `heuristic_fallback`) and an unparseable judge answer is recorded as *absent*, never as a silent `0.0`.
- Metering-friendly by design: spans carry byte/estimable payload; every recorded score is attributable (the two metrics Braintrust bills on).
