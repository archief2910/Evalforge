import { useEffect, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { api } from "../api.js";

export default function Datasets() {
  const { pid } = useParams();
  const nav = useNavigate();
  const [ds, setDs] = useState([]);
  const [form, setForm] = useState({ name: "", description: "" });
  const load = () => api.get(`/api/v1/projects/${pid}/datasets`).then(setDs).catch(() => {});
  useEffect(() => { load(); }, [pid]);
  const create = async () => {
    if (!form.name.trim()) return;
    const { id } = await api.post(`/api/v1/projects/${pid}/datasets`, form);
    setForm({ name: "", description: "" });
    load();
    nav(`/projects/${pid}/datasets/${id}`);
  };
  return (
    <div>
      <h1>Datasets</h1>
      <p className="sub">Versioned test cases: input + expected answer. Add records manually, or promote reviewed production traces.</p>
      <div className="toolbar">
        <input placeholder="Dataset name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
        <input placeholder="Description (optional)" style={{ width: 260 }} value={form.description}
          onChange={(e) => setForm({ ...form, description: e.target.value })} />
        <button onClick={create}>Create dataset</button>
      </div>
      <div className="panel" style={{ padding: 0 }}>
        <table>
          <thead><tr><th>Name</th><th>Records</th><th>Version</th><th>Created</th></tr></thead>
          <tbody>
            {ds.map((d) => (
              <tr key={d.id} className="clickable" onClick={() => nav(`/projects/${pid}/datasets/${d.id}`)}>
                <td style={{ fontWeight: 600 }}>{d.name}</td>
                <td>{d.record_count}</td>
                <td><span className="pill">v{d.version}</span></td>
                <td style={{ color: "var(--muted)" }}>{new Date(d.created_at).toLocaleDateString()}</td>
              </tr>
            ))}
            {!ds.length && <tr><td colSpan={4} className="empty">No datasets yet.</td></tr>}
          </tbody>
        </table>
      </div>
    </div>
  );
}
