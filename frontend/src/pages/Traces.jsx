import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api, fmt } from "../api.js";

export default function Traces() {
  const { pid } = useParams();
  const nav = useNavigate();
  const [data, setData] = useState({ traces: [], total: 0 });
  const [flt, setFlt] = useState({ search: "", status: "", model: "", has_feedback: "", sort_by: "start_ms", sort_dir: "desc" });
  const [models, setModels] = useState([]);

  const load = (f) => {
    const q = new URLSearchParams({ limit: 100 });
    for (const [k, v] of Object.entries(f || flt)) if (v) q.set(k, v);
    api.get(`/api/v1/projects/${pid}/traces?${q}`).then(setData).catch(() => {});
  };
  useEffect(() => { load(); api.get(`/api/v1/projects/${pid}/dashboard`).then((d) => setModels(d.model_breakdown || [])).catch(() => {}); }, [pid]);

  const set = (patch) => {
    const nf = { ...flt, ...patch };
    setFlt(nf);
    load(nf);
  };
  return (
    <div>
      <h1>Logs &amp; Traces</h1>
      <p className="sub">{fmt.num(data.total)} traces. Filter by status, model, text, feedback; sort by latency or cost.</p>
      <div className="toolbar">
        <input placeholder="Search input/output…" style={{ width: 220 }} value={flt.search}
          onChange={(e) => set({ search: e.target.value })} />
        <select value={flt.status} onChange={(e) => set({ status: e.target.value })}>
          <option value="">All statuses</option><option value="ok">ok</option><option value="error">error</option>
        </select>
        <select value={flt.model} onChange={(e) => set({ model: e.target.value })}>
          <option value="">All models</option>
          {models.filter((m) => m.model !== "none").map((m) => <option key={m.model} value={m.model}>{m.model}</option>)}
        </select>
        <label className="row" style={{ color: "var(--muted)", fontSize: 13 }}>
          <input type="checkbox" checked={!!flt.has_feedback} onChange={(e) => set({ has_feedback: e.target.checked ? "true" : "" })} />
          has human feedback
        </label>
        <div className="spacer" />
        <span style={{ color: "var(--muted)" }}>Sort</span>
        <select value={flt.sort_by} onChange={(e) => set({ sort_by: e.target.value })}>
          {["start_ms", "latency_ms", "cost_usd"].map((s) => <option key={s} value={s}>{s}</option>)}
        </select>
        <select value={flt.sort_dir} onChange={(e) => set({ sort_dir: e.target.value })}>
          <option value="desc">desc</option><option value="asc">asc</option>
        </select>
      </div>
      <div className="panel" style={{ padding: 0 }}>
        <table>
          <thead>
            <tr>
              <th>Input</th><th>Status</th><th>Latency</th><th>Spans</th><th>Model</th><th>Cost</th><th>Meta</th>
            </tr>
          </thead>
          <tbody>
            {data.traces.map((t) => (
              <tr key={t.trace_id} className="clickable" onClick={() => nav(`/projects/${pid}/traces/${t.trace_id}`)}>
                <td style={{ maxWidth: 320 }}>
                  <div className="mono" style={{ whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>
                    {typeof t.input === "string" ? t.input : JSON.stringify(t.input)}
                  </div>
                  <div style={{ color: "var(--muted)", fontSize: 11 }}>{t.name} · {new Date(t.start_ms).toLocaleString()}</div>
                </td>
                <td><span className={"pill " + (t.status === "error" ? "error" : "ok")}>{t.status}</span></td>
                <td>{fmt.ms(t.latency_ms)}</td>
                <td>{t.span_count}</td>
                <td className="mono" style={{ color: "var(--muted)" }}>{t.model || "–"}</td>
                <td>{fmt.usd(t.cost_usd)}</td>
                <td style={{ color: "var(--muted)", fontSize: 11 }}>
                  {t.metadata && Object.entries(t.metadata).slice(0, 3).map(([k, v]) => (
                    <div key={k}>{k}: {String(v)}</div>
                  ))}
                </td>
              </tr>
            ))}
            {!data.traces.length && <tr><td colSpan={7} className="empty">No traces match the filters.</td></tr>}
          </tbody>
        </table>
      </div>
    </div>
  );
}
