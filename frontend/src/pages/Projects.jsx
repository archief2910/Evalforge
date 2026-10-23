import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { api } from "../api.js";

export default function Projects() {
  const [projects, setProjects] = useState([]);
  const [name, setName] = useState("");
  const nav = useNavigate();
  const load = () => api.get("/api/v1/projects").then(setProjects).catch(() => {});
  useEffect(() => { load(); }, []);
  const create = async () => {
    if (!name.trim()) return;
    const { id } = await api.post("/api/v1/projects", { name: name.trim(), description: "" });
    setName("");
    load();
    nav(`/projects/${id}`);
  };
  return (
    <div>
      <h1>Projects</h1>
      <p className="sub">A project groups production logs, datasets, prompts and evaluation experiments.</p>
      <div className="toolbar">
        <input placeholder="New project name" value={name} onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && create()} />
        <button onClick={create}>Create project</button>
      </div>
      <div className="panel" style={{ padding: 0 }}>
        <table>
          <thead><tr><th>Name</th><th>Description</th><th>Created</th></tr></thead>
          <tbody>
            {projects.map((p) => (
              <tr key={p.id} className="clickable" onClick={() => nav(`/projects/${p.id}`)}>
                <td style={{ fontWeight: 600 }}>{p.name}</td>
                <td style={{ color: "var(--muted)" }}>{p.description}</td>
                <td style={{ color: "var(--muted)" }}>{new Date(p.created_at).toLocaleString()}</td>
              </tr>
            ))}
            {!projects.length && <tr><td colSpan={3} className="empty">No projects yet.</td></tr>}
          </tbody>
        </table>
      </div>
    </div>
  );
}
