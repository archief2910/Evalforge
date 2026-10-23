import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api } from "../api.js";

export default function DatasetDetail() {
  const { pid, dsid } = useParams();
  const nav = useNavigate();
  const [d, setD] = useState(null);
  const [form, setForm] = useState({ input: "", expected: "" });
  const [experiments, setExperiments] = useState([]);
  const [baseline, setBaseline] = useState("");
  const [scorers, setScorers] = useState([]);
  const [selScorers, setSelScorers] = useState(["exact_match", "factuality_judge"]);
  const [toast, setToast] = useState(null);
  const flash = (m) => { setToast(m); setTimeout(() => setToast(null), 3200); };

  const load = useCallback(() => {
    api.get(`/api/v1/datasets/${dsid}`).then(setD).catch(() => setD({}));
  }, [dsid]);
  useEffect(() => { load(); }, [load]);
  useEffect(() => {
    api.get(`/api/v1/projects/${pid}/experiments`).then(setExperiments).catch(() => {});
    api.get(`/api/v1/scorers?project_id=${pid}`).then((s) => setScorers(s)).catch(() => {});
  }, [pid]);

  if (!d) return <div className="empty">Loading…</div>;
  if (!d.records) return <div className="empty">Dataset not found. <Link to={`/projects/${pid}/datasets`}>Back</Link></div>;
  const addRecord = async () => {
    if (!form.input.trim()) return;
    let exp;
    try { exp = JSON.parse(form.expected); } catch (e) { exp = form.expected; }
    await api.post(`/api/v1/datasets/${dsid}/records`, { input: form.input, expected: exp });
    setForm({ input: "", expected: "" });
    load();
  };
  const runEval = async () => {
    const { experiment_id } = await api.post(`/api/v1/projects/${pid}/evaluations`, {
      dataset_id: dsid,
      experiment_name: `${d.name} ${new Date().toLocaleTimeString()}`,
      scorer_names: selScorers,
      baseline_experiment_id: baseline || null,
    });
    flash("Evaluation running…");
    nav(`/projects/${pid}/experiments/${experiment_id}`);
  };
  return (
    <div>
      <div className="row"><Link to={`/projects/${pid}/datasets`} style={{ color: "var(--muted)" }}>← Datasets</Link></div>
      <h1>{d.name} <span className="pill">v{d.version}</span></h1>
      <p className="sub">{d.description} {d.project_name && `· project: ${d.project_name}`} · {d.records.length} records</p>

      <div className="card">
        <b>Records</b>
        <table style={{ marginTop: 10 }}>
          <thead><tr><th style={{ width: "32%" }}>Input</th><th style={{ width: "32%" }}>Expected</th><th>Metadata / tags</th></tr></thead>
          <tbody>
            {d.records.map((r) => (
              <tr key={r.id}>
                <td className="mono" style={{ whiteSpace: "pre-wrap" }}>{typeof r.input === "string" ? r.input : JSON.stringify(r.input)}</td>
                <td className="mono" style={{ whiteSpace: "pre-wrap", color: "var(--muted)" }}>{r.expected == null ? "–" : typeof r.expected === "string" ? r.expected : JSON.stringify(r.expected)}</td>
                <td style={{ fontSize: 12, color: "var(--muted)" }}>
                  {r.source === "review" && <span className="pill score">from trace</span>}
                  {(r.metadata?.category || "") && <span className="pill">{r.metadata.category}</span>}
                  {(r.tags || []).map((t) => <span key={t} className="pill" style={{ marginRight: 4 }}>{t}</span>)}
                  {r.metadata?.origin_trace_id && (
                    <Link to={`/projects/${pid}/traces/${r.metadata.origin_trace_id}`} style={{ fontSize: 11 }}>view origin trace →</Link>
                  )}
                </td>
              </tr>
            ))}
            {!d.records.length && <tr><td colSpan={3} className="empty">No records. Add one below or promote a trace from the log viewer.</td></tr>}
          </tbody>
        </table>
      </div>

      <div className="grid cols2">
        <div className="card">
          <b>Add record manually</b>
          <textarea rows={2} style={{ width: "100%", marginTop: 8 }} placeholder="input (question)"
            value={form.input} onChange={(e) => setForm({ ...form, input: e.target.value })} />
          <textarea rows={2} style={{ width: "100%", marginTop: 6 }} placeholder="expected (ideal answer)"
            value={form.expected} onChange={(e) => setForm({ ...form, expected: e.target.value })} />
          <button className="small" style={{ marginTop: 8 }} onClick={addRecord}>Add record</button>
        </div>
        <div className="card">
          <b>Run evaluation on this dataset</b>
          <div style={{ color: "var(--muted)", fontSize: 12, marginTop: 6 }}>Scorers applied to every output:</div>
          <div className="row" style={{ flexWrap: "wrap", marginTop: 6 }}>
            {scorers.map((s) => {
              const on = selScorers.includes(s.name);
              return (
                <button key={s.name} title={s.description || s.kind} className={`small ${on ? "" : "ghost"}`}
                  onClick={() => setSelScorers(on ? selScorers.filter((n) => n !== s.name) : [...selScorers, s.name])}>
                  {s.name}
                </button>
              );
            })}
          </div>
          <div style={{ color: "var(--muted)", fontSize: 12, marginTop: 10 }}>Baseline for comparison (optional):</div>
          <select style={{ width: "100%", marginTop: 6 }} value={baseline} onChange={(e) => setBaseline(e.target.value)}>
            <option value="">— no baseline —</option>
            {experiments.map((e) => <option key={e.id} value={e.id}>{e.name}</option>)}
          </select>
          <button style={{ marginTop: 10 }} onClick={runEval}>▶ Run evaluation</button>
        </div>
      </div>
      {toast && <div className="toast">{toast}</div>}
    </div>
  );
}
