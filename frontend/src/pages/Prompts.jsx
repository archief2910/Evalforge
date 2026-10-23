import { useEffect, useState } from "react";
import { useParams } from "react-router-dom";
import { api } from "../api.js";

export default function Prompts() {
  const { pid } = useParams();
  const [ps, setPs] = useState([]);
  const [form, setForm] = useState({ name: "", model: "gpt-4o-mini", system: "", user: "{{input}}" });
  const load = () => api.get(`/api/v1/projects/${pid}/prompts`).then(setPs).catch(() => {});
  useEffect(() => { load(); }, [pid]);
  const save = async () => {
    if (!form.name.trim() || !form.system.trim()) return;
    await api.post(`/api/v1/projects/${pid}/prompts`, {
      name: form.name, model: form.model,
      template: { messages: [
        { role: "system", content: form.system },
        { role: "user", content: form.user },
      ]},
      params: { temperature: 0.2 },
    });
    setForm({ name: "", model: "gpt-4o-mini", system: "", user: "{{input}}" });
    load();
  };
  return (
    <div>
      <h1>Prompts</h1>
      <p className="sub">Versioned prompt templates with model + parameters. Latest version is used at runtime.</p>
      <div className="grid cols2" style={{ alignItems: "start" }}>
        <div className="panel" style={{ padding: 0 }}>
          <table>
            <thead><tr><th>Name</th><th>v</th><th>Model</th><th>Template (system)</th></tr></thead>
            <tbody>
              {ps.map((p) => (
                <tr key={p.id}>
                  <td style={{ fontWeight: 600 }}>{p.name}</td>
                  <td><span className="pill">{p.version}</span></td>
                  <td className="mono">{p.model}</td>
                  <td className="mono" style={{ color: "var(--muted)", maxWidth: 380, whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>
                    {(p.template?.messages || []).map((m) => `[${m.role}] ${m.content}`).join("  |  ")}
                  </td>
                </tr>
              ))}
              {!ps.length && <tr><td colSpan={4} className="empty">No prompts saved yet.</td></tr>}
            </tbody>
          </table>
        </div>
        <div className="panel">
          <b>New prompt version</b>
          <div style={{ display: "grid", gap: 8, marginTop: 10 }}>
            <input placeholder="Name (same name = new version)" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
            <select value={form.model} onChange={(e) => setForm({ ...form, model: e.target.value })}>
              {["gpt-4o-mini", "gpt-4o", "gpt-4.1", "claude-sonnet-4", "llama-3.1-8b"].map((m) => <option key={m}>{m}</option>)}
            </select>
            <textarea rows={3} placeholder="System message" value={form.system} onChange={(e) => setForm({ ...form, system: e.target.value })} />
            <textarea rows={2} placeholder="User message (use {{input}})" value={form.user} onChange={(e) => setForm({ ...form, user: e.target.value })} />
            <button onClick={save}>Save prompt</button>
          </div>
        </div>
      </div>
    </div>
  );
}
