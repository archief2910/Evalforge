import { useCallback, useEffect, useState } from "react";
import { useParams, Link } from "react-router-dom";
import { api, fmt } from "../api.js";

function JsonBlock({ v }) {
  if (v == null) return <div style={{ color: "var(--muted)" }}>— empty —</div>;
  let text;
  try { text = JSON.stringify(v, null, 2); } catch (e) { text = String(v); }
  return <pre className="json">{text}</pre>;
}

function SpanRow({ span, onSelect, selected, depth = 0 }) {
  return (
    <>
      <div className={"tree-row " + (selected ? "selected" : "")} onClick={() => onSelect(span)} style={{ paddingLeft: depth ? 8 : 0 }}>
        <span className={"pill " + (span.type === "llm" ? "llm" : span.type === "tool" ? "tool" : span.type === "score" ? "score" : span.status === "error" ? "error" : "")}>
          {span.type}{span.status === "error" ? " ⚠" : ""}
        </span>
        <span style={{ fontWeight: 600 }}>{span.name}</span>
        {span.model && <span className="mono" style={{ color: "var(--muted)" }}>{span.model}</span>}
        <div style={{ flex: 1 }} />
        <div style={{ width: 120 }}>
          <div className="bar" style={{ width: `${Math.min(100, (span.latency_ms / 4000) * 100)}%` }} title={fmt.ms(span.latency_ms)} />
        </div>
        <span style={{ color: "var(--muted)", width: 70, textAlign: "right" }}>{fmt.ms(span.latency_ms)}</span>
        <span style={{ color: "var(--muted)", width: 90, textAlign: "right" }}>
          {span.type === "llm" ? `${fmt.num((span.prompt_tokens || 0) + (span.completion_tokens || 0))} tok` : ""}
        </span>
      </div>
      {(span.children || []).length > 0 && (
        <div className="tree-children">
          {span.children.map((c) => <SpanRow key={c.id} span={c} onSelect={onSelect} selected={selected} depth={depth + 1} />)}
        </div>
      )}
    </>
  );
}

export default function TraceDetail() {
  const { pid, tid } = useParams();
  const [d, setD] = useState(null);
  const [sel, setSel] = useState(null);
  const [review, setReview] = useState({ comment: "", thumb: null });
  const [datasets, setDatasets] = useState([]);
  const [ds, setDs] = useState("");
  const [expected, setExpected] = useState("");
  const [toast, setToast] = useState(null);
  const [tab, setTab] = useState("messages");

  const load = useCallback(() => {
    api.get(`/api/v1/projects/${pid}/traces/${tid}`).then((r) => {
      setD(r);
      setSel((s) => s || r.span_tree[0]);
      // prefill expected review output from root output
      if (!review.comment && r.span_tree[0]?.output != null) {
        try { setExpected(JSON.stringify(r.span_tree[0].output, null, 2)); } catch (e) { setExpected(String(r.span_tree[0].output)); }
      }
    }).catch(() => setD({}));
  }, [pid, tid]);
  useEffect(() => { load(); }, [load]);
  useEffect(() => { api.get(`/api/v1/projects/${pid}/datasets`).then((r) => { setDatasets(r); if (r.length) setDs(r[0].id); }).catch(() => {}); }, [pid]);

  if (!d) return <div className="empty">Loading…</div>;
  if (!d.span_tree) return <div className="empty">Trace not found. <Link to={`/projects/${pid}/traces`}>Back to logs</Link></div>;
  const fb = d.feedback || [];
  return (
    <div>
      <div className="row"><Link to={`/projects/${pid}/traces`} style={{ color: "var(--muted)" }}>← Logs</Link></div>
      <h1>Trace <span className="mono" style={{ fontSize: 13 }}>{tid.slice(0, 8)}</span></h1>
      <p className="sub">Nested spans with duration, tokens and cost. Review the trace and promote useful cases into a dataset.</p>
      <div className="grid cols2" style={{ alignItems: "start", gridTemplateColumns: "1.1fr 1fr" }}>
        <div className="panel tree">
          <h2 style={{ marginTop: 0 }}>Span timeline</h2>
          {d.span_tree.map((r) => <SpanRow key={r.id} span={r} onSelect={setSel} selected={sel && sel.id === r.id} />)}
        </div>
        <div>
          <div className="card">
            {sel && (
              <>
                <div className="row" style={{ justifyContent: "space-between" }}>
                  <div className="row">
                    <span className={"pill " + (sel.type === "llm" ? "llm" : "")}>{sel.type}</span>
                    <b>{sel.name}</b>
                    {sel.model && <span className="mono" style={{ color: "var(--muted)" }}>{sel.model}</span>}
                  </div>
                  <div className="kv">
                    {(sel.prompt_tokens || sel.completion_tokens) ? `${fmt.num(sel.prompt_tokens)} in · ${fmt.num(sel.completion_tokens)} out · ${fmt.usd(sel.cost_usd)}` : fmt.ms(sel.latency_ms)}
                  </div>
                </div>
                {sel.error && <pre className="json" style={{ borderColor: "rgba(255,107,107,.5)" }}>{JSON.stringify(sel.error, null, 2)}</pre>}
                <div className="tabs" style={{ marginTop: 10 }}>
                  <div className={"tab" + (tab === "messages" ? " active" : "")} onClick={() => setTab("messages")}>Messages</div>
                  <div className={"tab" + (tab === "raw" ? " active" : "")} onClick={() => setTab("raw")}>Raw</div>
                </div>
                {tab === "messages" ? (
                  <>
                    <div style={{ color: "var(--muted)", fontSize: 11, marginTop: 6 }}>INPUT</div>
                    <JsonBlock v={sel.input} />
                    <div style={{ color: "var(--muted)", fontSize: 11, marginTop: 8 }}>OUTPUT</div>
                    <JsonBlock v={sel.output} />
                  </>
                ) : (
                  <JsonBlock v={{ id: sel.id, name: sel.name, type: sel.type, model: sel.model, input: sel.input, output: sel.output, metadata: sel.metadata, start_ms: sel.start_ms, end_ms: sel.end_ms, prompt_tokens: sel.prompt_tokens, completion_tokens: sel.completion_tokens }} />
                )}
              </>
            )}
          </div>

          {sel && sel.id === d.span_tree[0]?.id && (
            <div className="card">
              <b>Review this trace</b>
              <div className="row" style={{ marginTop: 10 }}>
                <button className={"small " + (review.thumb === "up" ? "" : "ghost")} onClick={async () => {
                  await api.post(`/api/v1/projects/${pid}/traces/${tid}/feedback`, { kind: "thumbs", value: "up", comment: review.comment || null });
                  setReview({ ...review, thumb: "up" }); setToast("👍 Positive feedback saved"); setTimeout(() => setToast(null), 2500); load();
                }}>👍 Good</button>
                <button className={"small " + (review.thumb === "down" ? "" : "ghost")} onClick={async () => {
                  await api.post(`/api/v1/projects/${pid}/traces/${tid}/feedback`, { kind: "thumbs", value: "down", comment: review.comment || null });
                  setReview({ ...review, thumb: "down" }); setToast("👎 Negative feedback saved"); setTimeout(() => setToast(null), 2500); load();
                }}>👎 Bad</button>
              </div>
              <textarea rows={2} style={{ width: "100%", marginTop: 10 }} placeholder="What was wrong / right about this answer?"
                value={review.comment} onChange={(e) => setReview({ ...review, comment: e.target.value })} />
              <div style={{ marginTop: 10, color: "var(--muted)", fontSize: 12 }}>
                Promote this trace into a dataset for regression testing. Edit the <i>expected</i> answer below:
              </div>
              <textarea rows={4} value={expected} onChange={(e) => setExpected(e.target.value)} style={{ width: "100%", marginTop: 6 }} />
              <div className="row" style={{ marginTop: 8 }}>
                <select value={ds} onChange={(e) => setDs(e.target.value)}>
                  {datasets.map((x) => <option key={x.id} value={x.id}>{x.name}</option>)}
                </select>
                <button className="small" onClick={async () => {
                  if (!ds) { setToast("Create a dataset first"); setTimeout(() => setToast(null), 2500); return; }
                  let exp;
                  try { exp = JSON.parse(expected); } catch (e) { exp = expected; }
                  await api.post(`/api/v1/datasets/${ds}/records:from_trace`, { trace_id: tid, expected: exp });
                  setToast("Added to dataset → now run an evaluation!"); setTimeout(() => setToast(null), 3000);
                }}>+ Add to dataset</button>
              </div>
            </div>
          )}

          {fb.length > 0 && (
            <div className="card">
              <b>Feedback history</b>
              <table style={{ marginTop: 8 }}>
                <tbody>
                  {fb.map((f) => (
                    <tr key={f.id}>
                      <td>{f.value === "up" ? "👍" : f.value === "down" ? "👎" : f.value}</td>
                      <td style={{ color: "var(--muted)" }}>{f.comment}</td>
                      <td style={{ color: "var(--muted)", fontSize: 11 }}>{new Date(f.created_at).toLocaleString()}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </div>
      {toast && <div className="toast">{toast}</div>}
    </div>
  );
}
