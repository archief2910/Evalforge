import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api, fmt } from "../api.js";

// Bucket width decides the axis format: daily buckets must be labelled with a
// date, otherwise every label repeats the same time of day.
function bucketStep(series) {
  if (series.length < 2) return 0;
  return series[1].ts - series[0].ts;
}

function bucketLabel(ts, step) {
  const d = new Date(ts);
  return step >= 24 * 3600 * 1000
    ? d.toLocaleDateString([], { month: "short", day: "numeric" })
    : d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

function BarChart({ series, metric }) {
  const max = Math.max(1, ...series.map((s) => s[metric]));
  const step = bucketStep(series);
  return (
    <div style={{ display: "flex", alignItems: "flex-end", gap: 3, height: 110 }}>
      {series.map((s, i) => (
        <div key={i} style={{ textAlign: "center", flex: 1 }}
          title={`${new Date(s.ts).toLocaleString()}: ${metric}=${fmt.num(s[metric])}`}>
          <div className="bar" style={{ height: Math.max(2, (s[metric] / max) * 100), width: "70%", margin: "0 auto" }} />
          <div style={{ color: "var(--muted)", fontSize: 9, marginTop: 4 }}>
            {bucketLabel(s.ts, step)}
          </div>
        </div>
      ))}
      {!series.length && <div className="empty">No data in range</div>}
    </div>
  );
}

function LineChart({ series, metric, color = "var(--accent)" }) {
  const max = Math.max(1, ...series.map((s) => s[metric]));
  const W = 560, H = 130, P = 18;
  const step = bucketStep(series);
  const pts = series.map((s, i) => [
    P + (i / Math.max(1, series.length - 1)) * (W - 2 * P),
    H - P - (s[metric] / max) * (H - 2 * P),
  ]);
  return (
    <div>
      {series.length > 0 && (
        <>
          <svg viewBox={`0 0 ${W} ${H}`} style={{ width: "100%", height: 130 }}>
            {pts.map((p, i) => (
              <circle key={i} cx={p[0]} cy={p[1]} r="2.5" fill={color}>
                <title>{`${new Date(series[i].ts).toLocaleString()} — ${fmt.num(series[i][metric])}`}</title>
              </circle>
            ))}
            <polyline fill="none" stroke={color} strokeWidth="2" points={pts.map((p) => p.join(",")).join(" ")} />
          </svg>
          <div style={{ display: "flex", justifyContent: "space-between", color: "var(--muted)", fontSize: 9 }}>
            <span>{bucketLabel(series[0].ts, step)}</span>
            <span>{bucketLabel(series[series.length - 1].ts, step)}</span>
          </div>
        </>
      )}
      {!series.length && <div className="empty">No data in range</div>}
    </div>
  );
}

export default function Dashboard() {
  const { pid } = useParams();
  const [d, setD] = useState(null);
  const [hours, setHours] = useState(168);
  useEffect(() => {
    api.get(`/api/v1/projects/${pid}/dashboard?hours=${hours}`).then(setD).catch(() => setD({}));
  }, [pid, hours]);
  return (
    <div>
      <h1>Dashboard</h1>
      <p className="sub">Production observability: requests, latency, tokens, cost and errors.</p>
      <div className="toolbar">
        <span style={{ color: "var(--muted)" }}>Time range</span>
        {[24, 72, 168].map((h) => (
          <button key={h} className={"small" + (hours === h ? "" : " ghost")} onClick={() => setHours(h)}>
            {h === 168 ? "7 days" : `${h}h`}
          </button>
        ))}
        <div className="spacer" />
        <Link className="btn ghost" to={`/projects/${pid}/traces`}>Open logs →</Link>
      </div>
      {!d ? null : (
        <>
          <div className="grid cols4">
            <div className="panel stat"><div className="label">Requests</div><div className="value">{fmt.num(d.requests)}</div><div className="hint">{fmt.num(d.span_count)} spans ingested</div></div>
            <div className="panel stat"><div className="label">Error rate</div><div className="value" style={{ color: d.error_rate > 0.02 ? "var(--red)" : "var(--green)" }}>{fmt.pct(d.error_rate)}</div><div className="hint">{fmt.num(d.errors)} failed traces</div></div>
            <div className="panel stat"><div className="label">Latency p50 / p95</div><div className="value">{fmt.ms(d.latency?.p50)} <span style={{ color: "var(--muted)", fontSize: 13 }}>/ {fmt.ms(d.latency?.p95)}</span></div><div className="hint">p99 {fmt.ms(d.latency?.p99)} · mean {fmt.ms(d.latency?.mean)}</div></div>
            <div className="panel stat"><div className="label">Cost &amp; tokens</div><div className="value">{fmt.usd(d.cost_usd)}</div><div className="hint">{fmt.num(d.tokens)} tokens</div></div>
          </div>
          <div className="grid cols2" style={{ marginTop: 14 }}>
            <div className="panel"><h2 style={{ marginTop: 0 }}>Requests per bucket</h2><BarChart series={d.timeseries || []} metric="requests" /></div>
            <div className="panel"><h2 style={{ marginTop: 0 }}>Latency p95</h2><LineChart series={d.timeseries || []} metric="latency_p95" /></div>
          </div>
          <div className="panel" style={{ marginTop: 14 }}>
            <h2 style={{ marginTop: 0 }}>Model breakdown</h2>
            <table>
              <thead><tr><th>Model</th><th>LLM calls</th><th /></tr></thead>
              <tbody>
                {(d.model_breakdown || []).map((m) => (
                  <tr key={m.model}>
                    <td className="mono">{m.model}</td><td>{fmt.num(m.count)}</td>
                    <td><div className="bar" style={{ width: Math.max(2, (m.count / Math.max(1, d.requests)) * 100) + "%" }} /></td>
                  </tr>
                ))}
                {!(d.model_breakdown || []).length && <tr><td colSpan={3} className="empty">No LLM calls in range</td></tr>}
              </tbody>
            </table>
          </div>
        </>
      )}
    </div>
  );
}
