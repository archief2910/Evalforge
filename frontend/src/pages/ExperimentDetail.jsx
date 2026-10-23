import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api } from "../api.js";

const deltaCls = (d) => (d > 0.05 ? "pos" : d < -0.05 ? "neg" : "zero");
const deltaTxt = (d) => (d == null ? "" : d > 0 ? `+${d.toFixed(2)}` : d.toFixed(2));

// How a score was produced - surfaced so a fallback is never mistaken for a
// real model judgment.
const MODE_LABEL = {
  llm: "llm",
  deterministic: null,
  heuristic_fallback: "fallback",
  provider_error: "no-score",
  unparseable: "no-score",
  error: "no-score",
};
const MODE_CLS = { llm: "llm", heuristic_fallback: "tool", provider_error: "error", unparseable: "error", error: "error" };

export default function ExperimentDetail() {
  const { pid, eid } = useParams();
  const [e, setE] = useState(null);
  const [rows, setRows] = useState(null);
  const [baseline, setBaseline] = useState(null);
  // An evaluation is dispatched to the worker asynchronously, so a running
  // experiment is polled until it reaches a terminal state.
  useEffect(() => {
    let cancelled = false;
    let timer = null;
    const load = async () => {
      try {
        const data = await api.get(`/api/v1/experiments/${eid}`);
        if (cancelled) return;
        setE(data);
        if (data.status === "running" || data.status === "pending") {
          timer = setTimeout(load, 3000);
        }
      } catch {
        if (!cancelled) setE({});
      }
    };
    load();
    return () => { cancelled = true; clearTimeout(timer); };
  }, [eid]);

  useEffect(() => {
    if (e?.baseline_experiment_id) {
      api.get(`/api/v1/experiments/${eid}/compare?baseline_id=${e.baseline_experiment_id}`).then(setRows).catch(() => setRows(null));
      api.get(`/api/v1/experiments/${e.baseline_experiment_id}`).then(setBaseline).catch(() => setBaseline(null));
    } else {
      setRows(null);
      setBaseline(null);
    }
  }, [eid, e?.baseline_experiment_id, e?.status, e?.runs?.length]);

  if (!e) return <div className="empty">Loading…</div>;
  if (!e.runs) return <div className="empty">Experiment not found.</div>;
  const s = e.summary || { scores: {}, comparison: null };
  const scoreNames = Object.keys(s.scores || {});
  const rerun = async () => {
    const dsid = e.dataset_id;
    const dataset = await api.get(`/api/v1/datasets/${dsid}`);
    const { experiment_id } = await api.post(
      `/api/v1/projects/${e.project_id}/evaluations`,
      { dataset_id: dsid, experiment_name: `${e.name} (rerun)`, scorer_names: Object.keys(s.scores || ["exact_match"]), baseline_experiment_id: e.baseline_experiment_id }
    );
    // reload happens through nav below
    window.location.assign(`/projects/${e.project_id}/experiments/${experiment_id}`);
  };
  return (
    <div>
      <div className="row"><Link to={`/projects/${pid}/experiments`} style={{ color: "var(--muted)" }}>← Experiments</Link></div>
      <h1>{e.name} <span className={"pill " + (e.status === "completed" ? "ok" : e.status === "failed" ? "error" : "")}>{e.status}</span></h1>
      <p className="sub">
        Dataset: {e.dataset_name || (e.dataset_id || "").slice(0, 8) + "…"}
        {e.baseline_experiment_id && (
          <> · baseline: {baseline ? baseline.name : (e.baseline_experiment_id || "").slice(0, 8) + "…"}</>
        )}
        {e.status === "running" && <> · running — this page updates automatically</>}
      </p>

      <div className="grid cols4">
        {scoreNames.map((n) => (
          <div key={n} className="panel stat">
            <div className="label">{n}</div>
            <div className="value">{(s.scores[n].mean ?? 0).toFixed(2)}</div>
            <div className="hint">{s.scores[n].count} scored cases</div>
          </div>
        ))}
        {!scoreNames.length && <div className="panel stat"><div className="label">Scores</div><div className="empty">No scores yet</div></div>}
      </div>

      {s.comparison && (
        <div className="card" style={{ marginTop: 14 }}>
          <b>Comparison vs baseline “{baseline ? baseline.name : s.comparison.baseline}”</b>
          <table style={{ marginTop: 8 }}>
            <thead><tr><th>Scorer</th><th>Mean Δ</th><th>Improved cases</th><th>Regressed cases</th></tr></thead>
            <tbody>
              {Object.entries(s.comparison.scores).map(([n, v]) => (
                <tr key={n}>
                  <td>{n}</td>
                  <td className={deltaCls(v.mean)}>{deltaTxt(v.mean)}</td>
                  <td className="pos">+{v.improvements}</td>
                  <td className="neg">−{v.regressions}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div className="card" style={{ marginTop: 14 }}>
        <b>Per-case results {rows ? "(aligned with baseline by input)" : ""}</b>
        <table style={{ marginTop: 8 }}>
          <thead>
            <tr>
              <th style={{ width: "24%" }}>Input</th><th style={{ width: "22%" }}>Output</th>
              {scoreNames.map((n) => <th key={n} style={{ width: 90 }}>{n}{rows ? " / Δ" : ""}</th>)}
              <th style={{ width: 90 }}>Latency</th>
            </tr>
          </thead>
          <tbody>
            {(rows || e.runs).map((r, i) => (
              <tr key={r.id || i}>
                <td className="mono" style={{ whiteSpace: "pre-wrap" }}>{typeof r.input === "string" ? r.input : JSON.stringify(r.input)}</td>
                <td className="mono" style={{ whiteSpace: "pre-wrap", color: "var(--muted)" }}>
                  {(r.error && <span className="neg">error: {r.error}</span>) || (r.output == null ? "–" : typeof r.output === "string" ? r.output : JSON.stringify(r.output))}
                  {r.output_mode && r.output_mode !== "llm" && (
                    <span className="pill tool" style={{ marginLeft: 6 }} title="Output was not produced by the configured model">{r.output_mode.replace("_", " ")}</span>
                  )}
                </td>
                {scoreNames.map((n) => {
                  const sc = r.scores?.[n];
                  const d = r.delta?.[n];
                  return (
                    <td key={n} title={sc?.rationale || ""}>
                      <span style={{ color: (sc?.score ?? 0) >= 0.8 ? "var(--green)" : (sc?.score ?? 0) >= 0.4 ? "var(--amber)" : "var(--red)", fontWeight: 600 }}>
                        {sc?.score == null ? "n/a" : sc.score.toFixed(2)}
                      </span>
                      {sc?.judge_mode && MODE_LABEL[sc.judge_mode] && (
                        <span className={"pill " + (MODE_CLS[sc.judge_mode] || "")} style={{ marginLeft: 5 }} title={sc.rationale || ""}>
                          {MODE_LABEL[sc.judge_mode]}
                        </span>
                      )}
                      {d != null && <span className={` ${deltaCls(d)}`}> {deltaTxt(d)}</span>}
                    </td>
                  );
                })}
                <td style={{ color: "var(--muted)" }}>{r.latency_ms == null ? "–" : `${r.latency_ms}ms`}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
