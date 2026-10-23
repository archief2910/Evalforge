import { useEffect, useState } from "react";
import { NavLink, Link, useParams, useLocation } from "react-router-dom";
import { api } from "../api.js";

export function Sidebar() {
  const { pid } = useParams();
  const loc = useLocation();
  const [project, setProject] = useState(null);
  useEffect(() => {
    if (pid) api.get(`/api/v1/projects/${pid}`).then(setProject).catch(() => {});
  }, [pid]);

  return (
    <aside className="sidebar">
      <Link to="/" style={{ display: "block", marginBottom: 4 }}>
        <div style={{ fontWeight: 700, fontSize: 15, color: "var(--text)" }}>EvalForge</div>
        <div style={{ color: "var(--muted)", fontSize: 11 }}>trace → review → eval</div>
      </Link>
      {!pid && (
        <>
          <div className="nav-group">Projects</div>
          <span className="nav-item active">All projects</span>
          <div className="nav-item" style={{ color: "var(--muted)", fontSize: 12 }}>Select a project to explore its logs, datasets and evals.</div>
        </>
      )}
      {pid && (
        <>
          <div className="nav-group">Project</div>
          <Link className="nav-item" style={{ color: "var(--text)", fontWeight: 600 }} to={`/projects/${pid}`}>{project?.name || "…"}</Link>
          <div className="nav-group">Observe</div>
          <NavLink className="nav-item" to={`/projects/${pid}`} end={loc.pathname === `/projects/${pid}`}>Dashboard</NavLink>
          <NavLink className="nav-item" to={`/projects/${pid}/traces`}>Logs &amp; Traces</NavLink>
          <div className="nav-group">Evaluate</div>
          <NavLink className="nav-item" to={`/projects/${pid}/datasets`}>Datasets</NavLink>
          <NavLink className="nav-item" to={`/projects/${pid}/experiments`}>Experiments</NavLink>
          <NavLink className="nav-item" to={`/projects/${pid}/prompts`}>Prompts</NavLink>
          <div className="nav-group">Workflow</div>
          <div style={{ color: "var(--muted)", fontSize: 11.5, padding: "4px 10px", lineHeight: 1.6 }}>
            1. Inspect traces<br />
            2. Review &amp; add to dataset<br />
            3. Run evaluation<br />
            4. Compare vs baseline
          </div>
        </>
      )}
    </aside>
  );
}
