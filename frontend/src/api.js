const j = async (r) => {
  if (!r.ok) throw new Error(`${r.status} ${await r.text().catch(() => "")}`);
  return r.json();
};
export const api = {
  get: (p) => fetch(p).then(j),
  post: (p, body) =>
    fetch(p, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body || {}) }).then(j),
};

export const fmt = {
  ms: (v) => (v == null ? "–" : v >= 10000 ? `${(v / 1000).toFixed(1)}s` : `${Math.round(v)}ms`),
  num: (v) => (v == null ? "–" : Intl.NumberFormat().format(v)),
  usd: (v) => (v == null ? "–" : `$${Number(v) >= 1 ? v.toFixed(2) : v.toFixed(4)}`),
  pct: (v) => (v == null ? "–" : `${(v * 100).toFixed(1)}%`),
};
