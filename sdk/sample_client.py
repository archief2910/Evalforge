"""Reference client for the AI Eval/Observability Platform ingest API.

Demonstrates the SDK surface used to instrument an LLM/agent app:
  - Client(project_id)                    -> client
  - client.start_span(name, type, input=)  nested spans via an explicit stack
  - span.end(output=..., error=...)        finalizes the span and closes it
  - client.flush()                         batch upload (durability after HTTP 200)

Run:  python sample_client.py --project-id <existing project id>
"""
import argparse, itertools, json, time, urllib.request

BASE = "http://localhost:8000"

# time.time_ns() has ~15ms granularity on Windows, so two spans created in the
# same tick would collide on the server's primary key. The counter guarantees
# process-unique span ids.
_seq = itertools.count(1)


def _post(path, payload):
    req = urllib.request.Request(
        BASE + path, data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"}, method="POST",
    )
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.loads(resp.read().decode())


class Span:
    def __init__(self, client, name, type="task", parent=None, input=None, model=None, metadata=None):
        self.client = client
        self.id = f"{time.time_ns()}-{next(_seq)}"
        self.parent = parent
        self.name = name
        self.type = type
        self.input = input
        self.model = model
        self.metadata = metadata or {}
        self.t0 = int(time.time() * 1000)
        self.output = None
        self.error = None
        self._ended = False

    def end(self, output=None, error=None):
        """Finalize the span. Closing a span pops it (and anything still open
        below it) from the client's stack, so the next span attaches to the
        right parent."""
        if self._ended:
            return
        self._ended = True
        if output is not None:
            self.output = output
        if error is not None:
            self.error = error
        self.client.end_span(self)
        self.client._buffer.append({
            "id": self.id,
            "parent_id": self.parent.id if self.parent else None,
            "trace_id": None,  # server resolves via parent / root rule
            "name": self.name, "type": self.type, "model": self.model,
            "input": self.input, "output": self.output, "error": self.error,
            "metadata": self.metadata or (self.parent.metadata if self.parent else None),
            "start_ms": self.t0, "end_ms": int(time.time() * 1000),
        })

    def __enter__(self):
        return self

    def __exit__(self, *a):
        if not self._ended:
            self.end()


class Client:
    def __init__(self, project_id):
        self.project_id = project_id
        self._stack = []
        self._buffer = []

    def start_span(self, name, type="task", input=None, model=None, metadata=None):
        parent = self._stack[-1] if self._stack else None
        s = Span(self, name, type=type, parent=parent, input=input, model=model, metadata=metadata)
        self._stack.append(s)
        return s

    def end_span(self, span):
        """Pop the span and any spans still open beneath it."""
        for i, s in enumerate(self._stack):
            if s is span:
                del self._stack[i:]
                return

    def flush(self):
        """Batched insert; durability begins only after HTTP 200 (retry in production)."""
        if not self._buffer:
            return 0
        batch, self._buffer = self._buffer, []
        return _post(f"/api/v1/projects/{self.project_id}/spans:ingest", {"events": batch})


def main(project_id):
    c = Client(project_id)
    with c.start_span("ask_support_agent", input="How do I reset my password?") as root:
        with c.start_span("retrieve_docs", type="tool", input={"query": "reset password"}) as retr:
            time.sleep(0.05)
            retr.end(output={"doc_id": "reset_password", "snippet": "Click 'Forgot password'..."})
        with c.start_span("generate_answer", type="llm", model="gpt-4o-mini",
                          input=["Click 'Forgot password'...", "How do I reset my password?"]) as llm:
            time.sleep(0.2)
            llm.end(output="Click 'Forgot password' on the login page and follow the emailed link.")
        root.output = llm.output
    print(c.flush())


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--project-id", required=True)
    args = ap.parse_args()
    main(args.project_id)
