"""Seed the Go server with realistic demo data.

The Go server owns the database; this script only talks to its HTTP API, so
seeding is a pure client concern (same contract an external tool would use).

Usage:  python worker-py/seed.py [--base http://localhost:8000]
"""
import argparse, json, random, sys, time, urllib.request

KB = [
    ("reset password", "Use the account recovery flow: click 'Forgot password' and follow the email link."),
    ("refund status", "Refunds post within 5-7 business days to the original payment method."),
    ("change plan", "You can upgrade in Settings > Billing > Change plan; proration is automatic."),
    ("api 401 error", "A 401 means the API key is invalid or revoked. Regenerate it in Settings > API keys."),
    ("cancel subscription", "Cancel anytime in Settings > Billing > Cancel; access lasts until period end."),
]

# Query intent -> KB topic. The demo only makes sense when each question is
# answered with the matching document, so every phrasing is mapped explicitly
# instead of falling back to a random topic.
ALIASES = {
    "reset password": ["reset my password", "reset password", "forgot password"],
    "refund status": ["refund status", "refund yet"],
    "change plan": ["change my plan", "change plan"],
    "api 401 error": ["401"],
    "cancel subscription": ["cancel my subscription", "cancel sub"],
}

QUERIES = [
    "How do I reset my password?", "Where do I check my refund status?", "How can I change my plan?",
    "I keep getting a 401 from the API", "How do I cancel my subscription?",
    "Reset password steps please", "my api returns 401 always why", "can u cancel sub for me",
    "change plan to annual", "refund yet not arrived", "I forgot password what now", "401 on every call",
]

MODELS = ["gpt-4o-mini", "gpt-4o-mini", "gpt-4o", "claude-sonnet-4"]

BASE = "http://localhost:8000"
_seq = 0


def new_id():
    global _seq
    _seq += 1
    return f"{time.time_ns()}-{_seq}"


def call(method, path, payload=None):
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(
        BASE + path, data=data, method=method,
        headers={"Content-Type": "application/json"} if data else {},
    )
    with urllib.request.urlopen(req, timeout=30) as r:
        body = r.read().decode()
        return json.loads(body) if body else {}


def seed():
    random.seed(42)
    projects = call("GET", "/api/v1/projects")
    if any(p["name"] == "Demo Support Agent" for p in projects):
        print("project already exists - nothing to seed")
        return 0

    pid = call("POST", "/api/v1/projects", {
        "name": "Demo Support Agent",
        "description": "Support bot: retrieve KB answer, grade response",
    })["id"]
    print("created project", pid)

    for name, model, template, params in [
        ("support-answer-v1", "gpt-4o-mini", {"messages": [
            {"role": "system", "content": "You are a helpful support agent. Answer concisely using the retrieved context."},
            {"role": "user", "content": "Retrieved context:\n{{input}}\n\nAnswer the customer question."},
        ]}, {"temperature": 0.2}),
        ("support-answer-v2", "gpt-4o", {"messages": [
            {"role": "system", "content": "You are a meticulous support agent. Cite the doc section id in your answer."},
            {"role": "user", "content": "Retrieved context:\n{{input}}\n\nAnswer the customer question with a doc reference."},
        ]}, {"temperature": 0.1}),
    ]:
        call("POST", f"/api/v1/projects/{pid}/prompts",
             {"name": name, "model": model, "template": template, "params": params})

    # Spread the traces over ~6 days so the dashboard timeseries (daily buckets
    # over the default 7-day window) shows a real curve, and every seeded trace
    # still falls inside that window.
    base = int(time.time() * 1000) - 6 * 24 * 3600 * 1000
    step = 4 * 3600 * 1000
    for i, q in enumerate(QUERIES * 3):
        trace_start = base + i * step + random.randint(-600_000, 600_000)
        model = random.choice(MODELS)
        root_id = new_id()
        kb_q = next((k for k in KB if any(a in q.lower() for a in ALIASES[k[0]])), None) or random.choice(KB)
        retrieved = f"doc:{kb_q[0].replace(' ', '_')} :: {kb_q[1]}"

        events = [{
            "id": root_id, "trace_id": root_id, "name": "support_agent", "type": "task",
            "input": q, "start_ms": trace_start,
            "metadata": {"environment": "production", "user_id": f"u{random.randint(1, 40)}",
                         "channel": random.choice(["web", "api", "slack"])},
        }]
        t_retr = trace_start + 30
        retr_span = {
            "id": new_id(), "trace_id": root_id, "parent_id": root_id,
            "name": "retrieve_docs", "type": "tool",
            "input": {"query": q},
            "output": {"query": q, "doc_id": kb_q[0].replace(" ", "_"), "snippet": kb_q[1]},
            "start_ms": t_retr, "end_ms": t_retr + random.randint(60, 400),
            "metadata": {"index": "kb_v2"},
        }
        if random.random() < 0.05:
            retr_span["error"] = {"message": "vector index timeout", "type": "tool_error"}
            retr_span["output"] = None
        events.append(retr_span)
        ok = not retr_span.get("error")

        llm_span = {
            "id": new_id(), "trace_id": root_id, "parent_id": root_id,
            "name": "generate_answer", "type": "llm", "model": model,
            "input": [retrieved, q],
            "start_ms": retr_span["end_ms"], "end_ms": retr_span["end_ms"] + random.randint(500, 3500),
        }
        if ok:
            h = sum(ord(c) for c in q) % 10
            if h < 5:
                ans = kb_q[1]
            elif h < 7:
                ans = f"I believe you can find this in your account settings. {kb_q[1]}"
            else:
                ans = "Sorry, I could not find relevant information about your request."
            if model == "claude-sonnet-4":
                ans += f" (doc ref: {kb_q[0].replace(' ', '_')})"
            llm_span["output"] = ans
        else:
            llm_span["error"] = {"message": "provider rate limit exceeded", "type": "provider_error"}
        events.append(llm_span)

        if ok and random.random() < 0.3:
            events.append({
                "id": new_id(), "trace_id": root_id, "parent_id": root_id,
                "name": "verify_tone", "type": "score",
                "input": llm_span.get("output"),
                "output": {"tone_pass": True, "score": round(random.uniform(0.7, 1.0), 2)},
                "start_ms": llm_span["end_ms"], "end_ms": llm_span["end_ms"] + random.randint(20, 120),
            })
        if ok:
            events[0]["output"] = llm_span.get("output")

        call("POST", f"/api/v1/projects/{pid}/spans:ingest", {"events": events})

        if random.random() < 0.35 and ok:
            good = random.random() < 0.7
            call("POST", f"/api/v1/projects/{pid}/traces/{root_id}/feedback", {
                "kind": "thumbs", "value": "up" if good else "down",
                "comment": None if good else random.choice(
                    ["missed the retrieval", "answer too vague", "wrong doc cited"]),
            })

    ds = call("POST", f"/api/v1/projects/{pid}/datasets", {
        "name": "Support regression cases", "description": "Cases captured from production review"})["id"]
    call("POST", f"/api/v1/datasets/{ds}/records", [
        {"input": "How do I reset my password?", "expected": KB[0][1], "metadata": {"category": "auth"}, "tags": ["regression"]},
        {"input": "Where do I check my refund status?", "expected": KB[1][1], "metadata": {"category": "billing"}, "tags": ["regression"]},
        {"input": "How can I change my plan?", "expected": KB[2][1], "metadata": {"category": "billing"}, "tags": ["regression"]},
        {"input": "I keep getting a 401 from the API", "expected": KB[3][1], "metadata": {"category": "api"}, "tags": ["regression"]},
        {"input": "How do I cancel my subscription?", "expected": KB[4][1], "metadata": {"category": "billing"}, "tags": ["regression"]},
    ])

    ds2 = call("POST", f"/api/v1/projects/{pid}/datasets", {
        "name": "API error FAQ", "description": "Upload from the CSV import"})["id"]
    call("POST", f"/api/v1/datasets/{ds2}/records", [
        {"input": "What does 401 mean on the API?", "expected": "/reset the key | invalid or revoked", "metadata": {"category": "api"}},
        {"input": "Can I use two API keys?", "expected": "Yes, create multiple keys in Settings > API keys with distinct scopes.", "metadata": {"category": "api"}},
    ])

    print(f"seeded {len(QUERIES * 3)} traces, 2 datasets, 2 prompts into project {pid}")
    return 0


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://localhost:8000")
    args = ap.parse_args()
    BASE = args.base
    try:
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    except Exception:
        pass
    sys.exit(seed())
