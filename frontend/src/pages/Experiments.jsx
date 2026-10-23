import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api, fmt } from "../api.js";

export default function Experiments() {
  const { pid } = useParams();
  const nav = useNavigate();
  const [exps, setExps] = useState([]);
  useEffect(() => { api.get(`/api/v1/projects/${pid}/experiments`).then(setExps).catch(() => {}); }, [pid]);
  const scorePills = (s) => (s?.scores ? Object.entries(s.scores).map(([n, v]) => (
    <span key={n} className="pill" style={{ marginRight: 6 }} title={`mean ${v.mean} (${v.count} runs)`}>{n}: {v.mean.toFixed(2)}</span>
  )) : null);
  return (
    <div>
      <h1>Experiments</h1>
      <p className="sub">Immutable records of evaluation runs. Open one to compare per-case results against its baseline.</p>
      {exps.some((e) => e.status === "running") && (
        <div className="review-banner">Some evaluations are still running — reload in a few seconds.</div>
      )}
      <div className="panel" style={{ padding: 0 }}>
        <table>
          <thead><tr><th>Name</th><th>Dataset</th><th>Status</th><th>Runs</th><th>Avg scores</th><th>vs baseline</th><th>Created</th></tr></thead>
          <tbody>
            {exps.map((e) => {
              const cmp = e.summary?.comparison;
              return (
                <tr key={e.id} className="clickable" onClick={() => nav(`/projects/${pid}/experiments/${e.id}`)}>
                  <td style={{ fontWeight: 600 }}>{e.name}</td>
                  <td style={{ color: "var(--muted)" }}>{e.dataset_name || "–"}</td>
                  <td><span className={"pill " + (e.status === "completed" ? "ok" : e.status === "failed" ? "error" : "")}>{e.status}</span></td>
                  <td>{e.run_count}</td>
                  <td>{scorePills(e.summary)}</td>
                  <td>
                    {cmp ? Object.entries(cmp.scores).map(([n, v]) => (
                      <span key={n} className={`pill ${v.improvements > v.regressions ? "good" : v.regressions > 0 ? "bad" : ""}`} title={`${n}: +${v.improvements} / -${v.regressions} cases`}>
                        {n}: Δ{v.mean >= 0 ? "+" : ""}{v.mean.toFixed(2)}
                      </span>
                    )) : <span style={{ color: "var(--muted)" }}>–</span>}
                  </td>
                  <td style={{ color: "var(--muted)" }}>{new Date(e.created_at).toLocaleString()}</td>
                </tr>
              );
            })}
            {!exps.length && <tr><td colSpan={7} className="empty">No experiments. Open a dataset and click “Run evaluation”.</td></tr>}
          </tbody>
        </table>
      </div>
    </div>
  );
}
