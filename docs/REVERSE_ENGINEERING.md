# Reverse-Engineering Braintrust → Design for "AI Eval/Observability Platform"

> All factual claims below come from Braintrust's official website, docs, GitHub repos, blog, and pricing pages
> (researched 2026-10). Items marked **[Inference]** are our own architectural deductions, not observed facts.
> This document drives the implementation of our **original** product. We do not copy Braintrust code, UI, or assets.

## Sources read
- https://www.braintrust.dev/ (homepage), /docs (incl. /docs/workflow, /instrument, /observe, /evaluate, /annotate)
- https://www.braintrust.dev/docs/reference/glossary
- https://www.braintrust.dev/docs/instrument/advanced-tracing (span record format)
- https://www.braintrust.dev/docs/api-reference (insert logs/experiments endpoints)
- https://www.braintrust.dev/docs/integrations/sdk-integrations/opentelemetry/send-traces-and-logs
- SDK repos: github.com/braintrustdata/braintrust-sdk-python, braintrust-sdk-javascript, autoevals
- https://www.braintrust.dev/pricing, /docs/plans-and-limits, /docs/admin/billing/faq, /docs/security
- Blog: /blog/ai-development-loops, /blog/eval-playgrounds, /blog/functions, /blog/loop, /blog/temporal-braintrust-integration

---

# PART 1 — OBSERVED PRODUCT MODEL (facts)

## 1.1 What the product is
Braintrust is an "active observability platform for agents". Its documented lifecycle is:

**Instrument → Observe → Annotate → Evaluate → Deploy → (monitor) → cycles back**

The key product insight: production traces and evaluation experiments use the *same data structure*, so real
production failures can be promoted into eval datasets and re-tested (the "feedback loop" the product is paid for).

## 1.2 Core objects
| Object | Documented role |
|---|---|
| Project | Container for logs, experiments, datasets, prompts, scorers/functions |
| Logs / Traces / Spans | Production interactions; a trace = end-to-end execution; spans = units of work, nested. Span types: task, llm, function, tool, score, classifier, eval, review |
| Dataset | Versioned collection of test cases: `input` (req), `expected`, `metadata`, `tags`; rows may *reference* a trace instead of copying it |
| Experiment | Immutable record of an offline eval run of a task over a dataset |
| Prompt | Versioned instructions + model config + messages, fetchable by slug at runtime |
| Scorers / Classifiers | Scorers → numeric 0..1 scores; classifiers → categorical labels. Sources: deterministic code, LLM-as-judge, prebuilt autoevals |
| Review | Human review queue over traces; add judgments, assign rows, promote to dataset |

## 1.3 Span record format (documented)
Stored span row: `id`, optional event data (`input`, `output`, `expected`, `scores`, `metadata`, `metrics`),
`span_id`, `root_span_id`, `span_parents` (list — DAG-capable), `span_attributes` (`name`, `type`).
Three distinct IDs: `id` = stored row, `span_id` = tree-construction ID (SDK-managed), `root_span_id` = trace ID.
UI displays single-root trees; data model allows multi-parent DAGs. Viewer has nested-Spans view, Timeline view,
per-span tabs (messages, details, review, activity, raw).

## 1.4 Ingestion architecture (documented)
- Direct: `POST /v1/project_logs/{project_id}/insert` (logs), `POST /v1/experiment/{experiment_id}/insert`
- OpenTelemetry: OTLP `POST /otel/v1/traces` (+ `/otel/v1/logs`), routing via `x-bt-parent: project_id:...`
- SDKs buffer in memory, batch upload in background; `flush()` required before exit; retry + queue-size limits;
  durability begins when the ingest API returns success.

## 1.5 Evaluation model (documented)
`Eval(name, data, task, scores, experiment_name, base_experiment_name, ...)`:
- runs `task(input)` per dataset case, applies scorers (`input`, `output`, `expected`, `metadata`) → score object or 0..1 number
- every run is stored as an immutable experiment; comparison aligns cases by `input` (custom comparison key possible)
- `experiment.summarize()` → per-score `improvements`, `regressions`, `diff` vs baseline

Autoevals scorer families: deterministic (`ExactMatch`, `Levenshtein`, `NumericDiff`, `JSONDiff`, `ValidJSON`),
embedding (`EmbeddingSimilarity`), RAG (`Faithfulness`, `ContextPrecision`/`Recall`, `AnswerRelevancy`...),
LLM-judge (`Factuality`, `ClosedQA`, `Battle`, `Moderation`, `Humor`, `Security`, `Sql`, `Summary`, `Translation`).

## 1.6 Business model (documented)
- Starter $0: $10 model credits, 1 GB processed data/mo (then $4/GB), 10k scores/mo, 14-day retention
- Pro $249/mo: $100 credits, 5 GB (then $3/GB), 50k scores/mo, 30-day retention (+$0.50/GB/mo extra)
- Enterprise: custom; hosted / BYOC / self-hosted data plane; SOC 2 Type II
- **Meters = processed-data bytes + recorded scores, NOT seats** (unlimited users). monetization hook is the
  integrated loop + enterprise deployment/compliance.

## 1.7 Competitive context
Langfuse (open-source, unit-per-trace+observation+score), LangSmith (seat + traces), Arize Phoenix (FOSS/OTel),
W&B Weave (GB-based). Braintrust's wedge = production→eval feedback loop + enterprise deployment options.

---

# PART 2 — INFERRED ARCHITECTURE [Inference]

Typical Braintrust-like backend combines:
1. **Ingest tier** — thin, write-optimized HTTP API accepting batched span events; schema validation; OTel mapping layer.
2. **Storage** — analytical storage (columnar/ClickHouse-like) for traces; transactional store for metadata objects
   (projects/datasets/experiments). We infer object storage for large blobs/attachments.
3. **Compute** — eval runners as queued jobs (durable execution); LLM-judge calls fan out to providers.
4. **Query layer** — filtered search over spans (BTQL-ish filter language), aggregation endpoints for dashboards.
5. **Control plane vs data plane split** — self-hosted data plane keeps trace data in-customer VPC; control plane
   handles auth/UI. Enterprise willingness-to-pay is mostly here.

**Our inference of the minimal durable design:** a relational schema is sufficient to deliver the whole loop at
small scale; the expensive part is correct span-tree modeling + filterable metadata + score attribution.

---

# PART 3 — OUR PRODUCT: AI Eval/Observability Platform

## 3.1 Original data model (our own schema — functionally similar, not copied)

```
projects            id, name, created_at, kind (production|playground)
spans               id, trace_id, parent_id, name, type(llm|tool|function|task|eval),
                    input(json), output(json), error(text), model, metadata(json),
                    start_ms, end_ms, latency_ms, prompt_tokens, completion_tokens,
                    cost_usd, status(ok|error), project_id, created_at
trace_meta          filled from root spans (aggregates denormalized for fast list views):
                    trace list row = meta + root span
feedback            id, trace_id, span_id, kind(thumbs|score|note), value, comment, created_at
datasets            id, project_id, name, version(int), description, created_at
dataset_records     id, dataset_id, input(json), expected(json), metadata(json), tags(json),
                    source(trace_ref|manual|upload), created_at
scorers             id, project_id, name, kind(deterministic|llm_judge), config(json),
                    description
experiments         id, project_id, name, dataset_id, baseline_experiment_id, status,
                    created_at, summary(json: per-score means, improvements, regressions, diff)
experiment_suites   (optional grouping of experiments)
eval_runs           id, experiment_id, record_id, input, expected, output, error,
                    latency_ms, scores(json: name->{score,reason}), created_at
prompts             id, project_id, name, version, model, template(json messages),
                    params(json temperature/max_tokens), created_at
```

Trace promotion: when a trace is reviewed and added to a dataset, we store
`input = trace root span's input`, `expected = user-supplied corrected output`, plus
`metadata.origin_trace_id` referencing the original trace (reference-style promotion, not copy-heavy).

## 3.2 API design (original, versioned under /api/v1)

Ingestion:
- `POST /api/v1/projects/{project_id}/spans:ingest` — batch of span events; server computes trace aggregates,
  latency, derives cost from a model price table; root spans (no parent) become traces.
- (OTel-compatible OTLP endpoint is out of MVP scope; noted as roadmap.)

Query/observe:
- `GET /api/v1/projects/{id}/traces?filter=json&limit&offset` — filters: model, status=error, latency/token/cost
  thresholds, time range, metadata equality, has_feedback; text search over input/output.
- `GET /api/v1/traces/{trace_id}` — full span tree
- `POST /api/v1/spans/{span_id}/feedback`, `POST /api/v1/traces/{trace_id}/feedback`
- `GET /api/v1/projects/{id}/dashboard?range=` — totals: requests, p50/p95/p99 latency, token totals,
  cost, error rate, model split, timeseries buckets.

Annotate / datasets:
- `POST /api/v1/datasets`, `GET /api/v1/datasets`, `GET /api/v1/datasets/{id}` (+records)
- `POST /api/v1/datasets/{id}/records` (manual / copied input+expected; accepts a single record or an array)
- `POST /api/v1/datasets/{id}/records:from_trace` {trace_id, expected?} — the review→dataset promotion

Evaluate:
- `GET/POST /api/v1/scorers` — built-in deterministic scorers auto-registered; custom LLM-judge scorers with
  criteria template; judge execution uses configured provider or deterministic fallback
- `POST /api/v1/projects/{id}/evaluations` {dataset_id, prompt_id?, task=server-side agent config, scorer_ids,
  experiment_name, baseline_experiment_id?} → async job; returns experiment id
- `GET /api/v1/experiments/{id}` → runs + summary (per-score avg, improvements vs baseline, diff counts)
- `GET /api/v1/experiments/{id}/compare?baseline=` → aligned-by-input comparison rows

Prompts:
- `GET/POST /api/v1/projects/{id}/prompts` — versioned template, model, params (playground-lite)

## 3.3 Eval runner design [Inference]

Server-side runner executes: fetch dataset records → for each record render prompt template → call LLM provider
(supports any OpenAI-compatible base_url + key; works with local/mock provider for demo) → apply scorers →
write eval_runs + experiment row. Scores recorded per case; summary aggregates. No provider key? The runner
falls back to a deterministic echo/simulated mode so the full loop is demoable offline.

## 3.4 Deterministic scorers (built-in)
`exact_match`, `contains`, `starts_with`, `json_valid`, `numeric_diff`, `levenshtein_similarity`,
`regex_guard` (config: must_match/must_not_match). All take (output, expected) → 0..1.

## 3.5 LLM-as-a-judge scorers (built-in)
`factuality_judge`, `helpfulness_judge` — criteria template with {input} {output} {expected} placeholders,
asking model to return strict JSON `{score: 0..1, rationale}`. With no provider configured, a lexical-overlap
heuristic produces the score and marks `judge_mode: heuristic_fallback` so results are honest about how they
were scored.

## 3.6 MVP scope (what we build now)
1. Go + SQLite API server implementing all APIs above (batch span ingest, filtered trace search, trace tree,
   feedback, dashboard aggregates, datasets + trace promotion, scorer registry, async eval runner, experiment
   comparison, prompts), with a Python worker process for eval execution.
2. React + Vite frontend: Projects, Logs(traces) with filters, Trace viewer (nested spans tree + timeline +
   per-span I/O), Dashboard charts (latency/tokens/cost/errors, model split), Datasets page (add records +
   promote from trace), Experiments page (run eval wizard, comparison table vs baseline, improvement/regression
   badges), Prompts page.
3. `sdk/sample_client.py` — a ~100-line reference client demonstrating `init(project)`, `start_span` nesting,
   `flush()` batching semantics (the SDK surface, not Braintrust's code).
4. Seed script creating a demo project with realistic multi-span agent traces (some errors, mixed models) so the
   UI is immediately explorable and the full loop can be demonstrated in minutes.

## 3.7 Explicitly out of MVP (roadmap)
OTLP ingest path, per-seat auth/RBAC (single-tenant local for now), review queue assignment, alerts/monitors,
prompt playground diffs, dataset snapshots/versioning, BTQL query language (we ship a structured JSON filter),
embedding-similarity scorers, plugin SDK for custom code scorers (we ship config-driven + judge scorers).

## 3.8 Why users would pay (monetization thesis, grounded in 1.6)
- The loop, not the traces, is the product: trace → review → dataset → regression eval is the retention mechanic.
- Meter like the market already validates: generous free tier with **volume meters (ingested span bytes /
  recorded scores)**, paid tiers unlock retention + higher volume + judge credits; unlimited seats at every tier
  to remove collaboration friction.
- Enterprise wedge (later): on-prem/BYOC data plane, because "trace data stays in the customer's VPC" is what
  large buyers actually purchase (observed in Braintrust's self-hosting docs and pricing).

## 3.9 Technical decisions log
| Decision | Choice | Rationale |
|---|---|---|
| Backend | Go (`net/http` + `modernc.org/sqlite`, pure-Go driver) | one static binary, no cgo, no runtime deps; fast enough to sit in front of the ingest path |
| Storage | SQLite single file, JSON columns | MVP scale; spans queried by trace_id + structured filters |
| Frontend | React 19 + Vite, no UI framework | keep bundle small; CSS is original, not a copy of Braintrust's UI |
| Eval runner | Go dispatches a Python worker process; results return via a worker-callback API | keeps LLM/scorer code in Python without giving the API server an event loop; no external broker needed at MVP scale |
| LLM access | OpenAI-compatible env config (OpenRouter by default) | works with OpenAI, Together, local vLLM/Ollama; heuristic fallback keeps demo offline |
| IDs | `<unix-nanos>-<sequence>` strings | no coordination needed across ingest workers; the sequence suffix keeps ids unique on clocks with coarse resolution |
| Cost accounting | model price table (per 1K tokens) applied at ingest | cost charts need no recompute |
| API versioning | /api/v1, JSON filters | stable client surface for the sample SDK |
