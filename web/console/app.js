/* hpcdn console — dependency-free SPA.
   Untrusted strings (node names, notes, log lines) only ever enter the DOM
   via textContent. Charts are hand-rolled SVG following the house dataviz
   rules: 2px lines, 10% area wash, hairline grid, crosshair + all-series
   tooltip, legend for ≥2 series, text in ink tokens never series color. */

"use strict";

/* ---------- helpers ---------- */

const $ = (sel, root) => (root || document).querySelector(sel);

function el(tag, attrs, ...children) {
  const n = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (k === "class") n.className = v;
      else if (k === "text") n.textContent = v;
      else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
      else n.setAttribute(k, v);
    }
  }
  for (const c of children) {
    if (c == null) continue;
    n.append(c.nodeType ? c : document.createTextNode(c));
  }
  return n;
}

function svgEl(tag, attrs) {
  const n = document.createElementNS("http://www.w3.org/2000/svg", tag);
  if (attrs) for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v);
  return n;
}

function fmtBytes(b, suffix) {
  if (!isFinite(b) || b < 0) b = 0;
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (b >= 1024 && i < units.length - 1) { b /= 1024; i++; }
  return (b >= 100 ? b.toFixed(0) : b >= 10 ? b.toFixed(1) : b.toFixed(2)) + " " + units[i] + (suffix || "");
}

function fmtDur(sec) {
  sec = Math.max(0, Math.floor(sec));
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${sec % 60}s`;
  return `${sec}s`;
}

function fmtAgo(iso) {
  const t = new Date(iso).getTime();
  if (!t || t < 0) return "never";
  const s = (Date.now() - t) / 1000;
  if (s < 4) return "now";
  if (s < 90) return `${Math.round(s)}s ago`;
  if (s < 5400) return `${Math.round(s / 60)}m ago`;
  return `${Math.round(s / 3600)}h ago`;
}

function toast(msg) {
  const t = $("#toast");
  t.textContent = msg;
  t.classList.add("show");
  clearTimeout(toast._t);
  toast._t = setTimeout(() => t.classList.remove("show"), 2600);
}

/* ---------- API ---------- */

const api = {
  key: localStorage.getItem("hpcdn_key") || "",
  async req(method, path, body) {
    const opts = { method, headers: { Authorization: "Bearer " + this.key } };
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    const resp = await fetch(path, opts);
    if (resp.status === 401) { gate.show(); throw new Error("unauthorized"); }
    if (!resp.ok) {
      let msg = resp.statusText;
      try { msg = (await resp.json()).error || msg; } catch { /* not json */ }
      throw new Error(msg);
    }
    if (resp.status === 204) return null;
    return resp.json();
  },
  get: (p) => api.req("GET", p),
  post: (p, b) => api.req("POST", p, b ?? {}),
  put: (p, b) => api.req("PUT", p, b),
  del: (p) => api.req("DELETE", p),
};

/* ---------- login gate ---------- */

const gate = {
  show() {
    $("#gate").classList.remove("hidden");
    $("#gate-key").focus();
  },
  hide() { $("#gate").classList.add("hidden"); },
  async submit() {
    const key = $("#gate-key").value.trim();
    if (!key) return;
    api.key = key;
    try {
      await api.get("/api/v1/whoami");
      localStorage.setItem("hpcdn_key", key);
      $("#gate-err").textContent = "";
      gate.hide();
      route();
    } catch {
      $("#gate-err").textContent = "That key was rejected by the controller.";
    }
  },
};

/* ---------- theme ---------- */

function applyTheme(t) {
  document.documentElement.setAttribute("data-theme", t);
  localStorage.setItem("hpcdn_theme", t);
}
function initTheme() {
  const saved = localStorage.getItem("hpcdn_theme");
  applyTheme(saved || (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light"));
}

/* ---------- charts ---------- */

const SERIES = ["var(--s1)", "var(--s2)", "var(--s3)", "var(--s4)", "var(--s5)", "var(--s6)"];

/* lineChart renders a multi-series time chart with hairline grid, crosshair
   and an all-series tooltip. series: [{name, points:[{t,v}], fmt}] */
function lineChart(container, series, opts = {}) {
  container.textContent = "";
  const H = opts.height || 180, PADL = 64, PADR = 12, PADT = 10, PADB = 22;
  const W = Math.max(320, container.clientWidth || 640);
  const wrap = el("div", { class: "chart-wrap" });
  const svg = svgEl("svg", { viewBox: `0 0 ${W} ${H}`, width: "100%", height: H });
  wrap.append(svg);
  container.append(wrap);

  const all = series.flatMap(s => s.points);
  if (!all.length) {
    wrap.append(el("div", { class: "empty", text: "No telemetry yet — data appears as nodes report." }));
    return;
  }
  const t0 = Math.min(...all.map(p => p.t)), t1 = Math.max(...all.map(p => p.t));
  let vmax = opts.max ?? Math.max(...all.map(p => p.v), 1e-9);
  if (opts.max === undefined) vmax *= 1.15;
  const x = t => PADL + (W - PADL - PADR) * (t1 === t0 ? 0.5 : (t - t0) / (t1 - t0));
  const y = v => PADT + (H - PADT - PADB) * (1 - v / vmax);

  const fmt = opts.fmt || (v => v.toFixed(1));

  // grid: 3 hairlines + labels in muted ink
  for (let i = 0; i <= 3; i++) {
    const v = vmax * i / 3, gy = y(v);
    svg.append(svgEl("line", { x1: PADL, y1: gy, x2: W - PADR, y2: gy, stroke: "var(--grid)", "stroke-width": 1 }));
    const lbl = svgEl("text", { x: PADL - 7, y: gy + 3.5, "text-anchor": "end", "font-size": 10, fill: "var(--muted)" });
    lbl.textContent = fmt(v);
    svg.append(lbl);
  }
  // time labels
  for (const frac of [0, 0.5, 1]) {
    const tt = t0 + (t1 - t0) * frac;
    const lbl = svgEl("text", {
      x: x(tt), y: H - 6, "text-anchor": frac === 0 ? "start" : frac === 1 ? "end" : "middle",
      "font-size": 10, fill: "var(--muted)",
    });
    lbl.textContent = new Date(tt).toLocaleTimeString([], { hour12: false, hour: "2-digit", minute: "2-digit", second: "2-digit" });
    svg.append(lbl);
  }

  series.forEach((s, i) => {
    if (!s.points.length) return;
    const color = SERIES[i % SERIES.length];
    const pts = s.points.map(p => `${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join(" ");
    if (opts.area && series.length === 1) {
      const first = s.points[0], last = s.points[s.points.length - 1];
      const path = `M ${x(first.t)},${y(0)} L ${pts.replaceAll(" ", " L ")} L ${x(last.t)},${y(0)} Z`;
      svg.append(svgEl("path", { d: path, fill: color, opacity: 0.1 }));
    }
    svg.append(svgEl("polyline", {
      points: pts, fill: "none", stroke: color,
      "stroke-width": 2, "stroke-linejoin": "round", "stroke-linecap": "round",
    }));
    const last = s.points[s.points.length - 1];
    svg.append(svgEl("circle", {
      cx: x(last.t), cy: y(last.v), r: 4, fill: color,
      stroke: "var(--surface)", "stroke-width": 2,
    }));
  });

  // crosshair + tooltip
  const cross = svgEl("line", { y1: PADT, y2: H - PADB, stroke: "var(--baseline)", "stroke-width": 1, visibility: "hidden" });
  svg.append(cross);
  const tip = el("div", { class: "tooltip" });
  wrap.append(tip);

  svg.addEventListener("pointermove", ev => {
    const rect = svg.getBoundingClientRect();
    const mx = (ev.clientX - rect.left) * (W / rect.width);
    if (mx < PADL || mx > W - PADR) { cross.setAttribute("visibility", "hidden"); tip.style.display = "none"; return; }
    // snap to nearest timestamp across the densest series
    const ref = series.reduce((a, b) => (b.points.length > (a?.points.length || 0) ? b : a), null);
    if (!ref || !ref.points.length) return;
    let best = ref.points[0];
    for (const p of ref.points) if (Math.abs(x(p.t) - mx) < Math.abs(x(best.t) - mx)) best = p;
    const sx = x(best.t);
    cross.setAttribute("x1", sx); cross.setAttribute("x2", sx);
    cross.setAttribute("visibility", "visible");

    tip.textContent = "";
    tip.append(el("div", { class: "tt-time", text: new Date(best.t).toLocaleTimeString() }));
    series.forEach((s, i) => {
      let near = null;
      for (const p of s.points) if (!near || Math.abs(p.t - best.t) < Math.abs(near.t - best.t)) near = p;
      if (!near) return;
      tip.append(el("div", { class: "tt-row" },
        el("span", { class: "tt-key", style: `background:${SERIES[i % SERIES.length]}` }),
        el("span", { class: "tt-val", text: (s.fmt || fmt)(near.v) }),
        el("span", { class: "tt-name", text: s.name }),
      ));
    });
    tip.style.display = "block";
    const tipW = tip.offsetWidth || 140;
    const px = (sx / W) * rect.width;
    tip.style.left = Math.min(Math.max(px + 12, 0), rect.width - tipW - 4) + "px";
    tip.style.top = "8px";
  });
  svg.addEventListener("pointerleave", () => {
    cross.setAttribute("visibility", "hidden");
    tip.style.display = "none";
  });

  if (series.length >= 2) {
    const legend = el("div", { class: "legend" });
    series.forEach((s, i) => legend.append(
      el("span", { class: "key" },
        el("span", { class: "swatch-line", style: `background:${SERIES[i % SERIES.length]}` }),
        s.name),
    ));
    container.append(legend);
  }
}

/* sparkline — 12ish points, de-emphasized, no axes */
function sparkline(points, w = 84, h = 24) {
  const svg = svgEl("svg", { viewBox: `0 0 ${w} ${h}`, width: w, height: h });
  if (points.length < 2) return svg;
  const vmax = Math.max(...points, 1e-9);
  const step = w / (points.length - 1);
  const pts = points.map((v, i) => `${(i * step).toFixed(1)},${(h - 3 - (h - 6) * (v / vmax)).toFixed(1)}`).join(" ");
  svg.append(svgEl("polyline", {
    points: pts, fill: "none", stroke: "var(--baseline)", "stroke-width": 1.5,
    "stroke-linejoin": "round", "stroke-linecap": "round",
  }));
  const [lx, ly] = pts.split(" ").pop().split(",");
  svg.append(svgEl("circle", { cx: lx, cy: ly, r: 2.5, fill: "var(--accent)" }));
  return svg;
}

/* ---------- shared UI pieces ---------- */

function stateChip(n) {
  const cls = n.draining ? "draining" : n.state;
  const label = n.draining ? "draining" : n.state;
  return el("span", { class: `chip ${cls}` }, el("span", { class: "dot" }), label);
}

function meter(pct) {
  const p = Math.max(0, Math.min(100, pct || 0));
  const cls = p >= 90 ? "hot" : p >= 75 ? "warn" : "";
  return el("span", { class: "meter" },
    el("span", { class: "track" }, el("span", { class: `fill ${cls}`, style: `width:${p}%` })),
    el("span", { class: "pct", text: p.toFixed(0) + "%" }),
  );
}

function tile(label, value, sub) {
  const t = el("div", { class: "card tile" },
    el("div", { class: "t-label", text: label }),
    el("div", { class: "t-value" }, value),
  );
  if (sub) t.append(el("div", { class: "t-sub" }, sub));
  return t;
}

function copyBtn(text) {
  return el("button", {
    class: "btn small copy", text: "copy",
    onclick: async (ev) => {
      ev.stopPropagation();
      try { await navigator.clipboard.writeText(text); toast("Copied to clipboard"); }
      catch { toast("Copy failed — select manually"); }
    },
  });
}

/* ---------- pages ---------- */

const view = $("#view");
let refreshTimer = null;

function setRefresh(fn, ms) {
  clearInterval(refreshTimer);
  if (fn) refreshTimer = setInterval(() => { if (!document.hidden) fn().catch(() => {}); }, ms || 3000);
}

function topline(title, crumb) {
  const t = el("div", { class: "topline" },
    el("h1", null, crumb ? el("span", { class: "crumb" }, crumb + " / ") : null, title),
    el("div", { class: "topline-side" },
      el("span", { class: "conn", id: "conn" }, el("span", { class: "conn-dot" }), "live"),
    ),
  );
  return t;
}

function setConn(ok) {
  const c = $("#conn");
  if (!c) return;
  c.textContent = "";
  c.append(el("span", { class: "conn-dot" + (ok ? "" : " off") }), ok ? "live" : "disconnected");
}

/* ----- overview ----- */

async function pageOverview() {
  view.textContent = "";
  view.append(topline("Overview"));
  const tiles = el("div", { class: "grid tiles" });
  const chartCard = el("div", { class: "card" }, el("h2", { text: "Edge egress — bytes out per second" }));
  const chartBox = el("div");
  chartCard.append(chartBox);
  const feedCard = el("div", { class: "card" },
    el("h2", null, "Cluster events ", el("span", { class: "h2-side", text: "live via SSE" })));
  const feed = el("div", { class: "feed", id: "feed" });
  feedCard.append(feed);
  view.append(tiles, chartCard, feedCard);

  async function load() {
    const [stats, nodes] = await Promise.all([api.get("/api/v1/stats"), api.get("/api/v1/nodes")]);
    setConn(true);

    tiles.textContent = "";
    tiles.append(
      tile("Edge nodes healthy", el("span", null, String(stats.edges_healthy), el("small", null, " / " + stats.edges_total)),
        stats.edges_healthy === stats.edges_total && stats.edges_total > 0 ? el("span", { class: "up", text: "all nodes in rotation" }) : "check the nodes page"),
      tile("Routing throughput", el("span", null, stats.routed_per_sec.toFixed(1), el("small", null, " req/s")),
        `${stats.routed_total.toLocaleString()} sessions total`),
      tile("Cluster egress", el("span", null, fmtBytes(stats.bytes_out_rate, "/s")), "sum of all edges"),
      tile("Cache hit ratio", el("span", null, (stats.hit_ratio * 100).toFixed(1), el("small", null, "%")),
        fmtBytes(stats.cache_bytes) + " cached"),
      tile("Controller uptime", el("span", null, fmtDur(stats.uptime_seconds)), null),
    );

    const edges = nodes.filter(n => n.kind === "edge");
    const seriesData = await Promise.all(edges.slice(0, 6).map(async n => {
      const hist = await api.get(`/api/v1/nodes/${n.id}/telemetry`).catch(() => []);
      return {
        name: n.name,
        fmt: v => fmtBytes(v, "/s"),
        points: hist.map(s => ({ t: new Date(s.t).getTime(), v: s.out_rate })),
      };
    }));
    lineChart(chartBox, seriesData.filter(s => s.points.length), { fmt: v => fmtBytes(v, "/s"), area: true });
  }

  await load().catch(() => setConn(false));
  setRefresh(load, 3000);
  events.attach(feed);
}

/* ----- nodes ----- */

let nodeFilter = "all";

async function pageNodes() {
  view.textContent = "";
  view.append(topline("Nodes"));

  const seg = el("div", { class: "seg" });
  for (const k of ["all", "edge", "origin"]) {
    seg.append(el("button", {
      text: k === "all" ? "All" : k === "edge" ? "Edges" : "Origins",
      class: nodeFilter === k ? "on" : "",
      onclick: () => { nodeFilter = k; pageNodes(); },
    }));
  }
  view.append(el("div", { class: "controls" }, seg));

  const card = el("div", { class: "card" });
  view.append(card);

  async function load() {
    const nodes = (await api.get("/api/v1/nodes")).filter(n => nodeFilter === "all" || n.kind === nodeFilter);
    setConn(true);
    card.textContent = "";
    if (!nodes.length) {
      card.append(el("div", { class: "empty", text: "No nodes enrolled yet. Mint a join token on the Enroll page and start an edge or origin with it." }));
      return;
    }
    const tbl = el("table");
    tbl.append(el("thead", null, el("tr", null,
      ...["Node", "Kind", "Region", "State", "CPU", "RAM", "CPU trend", "Conns", "Egress", "Hit", "Last seen", ""]
        .map(h => el("th", { text: h })))));
    const tb = el("tbody");
    for (const n of nodes) {
      const spark = sparkline([]);
      api.get(`/api/v1/nodes/${n.id}/telemetry`).then(h => {
        const pts = h.slice(-24).map(s => s.cpu);
        if (pts.length >= 2) spark.replaceWith(sparkline(pts));
      }).catch(() => {});
      const row = el("tr", { class: "rowlink", onclick: () => { location.hash = `#/nodes/${n.id}`; } },
        el("td", null, el("div", { class: "td-name", text: n.name }), el("div", { class: "td-id", text: n.id })),
        el("td", { text: n.kind }),
        el("td", { text: n.region || "—" }),
        el("td", null, stateChip(n)),
        el("td", null, meter(n.cpu_percent)),
        el("td", null, meter(n.ram_percent)),
        el("td", null, spark),
        el("td", { class: "num", text: String(n.active_conns) }),
        el("td", { class: "num", text: fmtBytes(n.bytes_out_rate, "/s") }),
        el("td", { class: "num", text: (n.hit_ratio * 100).toFixed(0) + "%" }),
        el("td", { text: fmtAgo(n.last_seen) }),
        el("td", null, nodeActions(n, load)),
      );
      tb.append(row);
    }
    tbl.append(tb);
    card.append(tbl);
  }

  await load().catch(() => setConn(false));
  setRefresh(load, 3000);
}

function nodeActions(n, reload) {
  const box = el("span", { style: "display:inline-flex;gap:6px" });
  if (n.kind === "edge") {
    box.append(el("button", {
      class: "btn small", text: n.draining ? "undrain" : "drain",
      onclick: async ev => {
        ev.stopPropagation();
        await api.post(`/api/v1/nodes/${n.id}/${n.draining ? "undrain" : "drain"}`);
        toast(n.draining ? "Node returning to rotation" : "Node draining — no new sessions");
        reload();
      },
    }));
  }
  box.append(el("button", {
    class: "btn small danger", text: "remove",
    onclick: async ev => {
      ev.stopPropagation();
      if (!confirm(`Deregister ${n.name}? It can re-enroll with a join token.`)) return;
      await api.del(`/api/v1/nodes/${n.id}`);
      toast("Node removed");
      reload();
    },
  }));
  return box;
}

/* ----- node detail ----- */

async function pageNodeDetail(id) {
  view.textContent = "";
  const head = topline("…", "Nodes");
  view.append(head);

  const metaCard = el("div", { class: "card" });
  const hwCard = el("div", { class: "card" }, el("h2", { text: "Hardware — CPU / RAM / disk (%)" }));
  const hwBox = el("div"); hwCard.append(hwBox);
  const netCard = el("div", { class: "card" }, el("h2", { text: "Throughput — bytes/second" }));
  const netBox = el("div"); netCard.append(netBox);
  const grid = el("div", { class: "grid cols-2" }, hwCard, netCard);
  const connCard = el("div", { class: "card" }, el("h2", { text: "Active connections" }));
  const connBox = el("div"); connCard.append(connBox);
  view.append(metaCard, grid, connCard);

  async function load() {
    const [n, hist] = await Promise.all([
      api.get(`/api/v1/nodes/${id}`),
      api.get(`/api/v1/nodes/${id}/telemetry`),
    ]);
    setConn(true);
    head.replaceWith(topline(n.name, "Nodes"));

    metaCard.textContent = "";
    const kv = el("dl", { class: "kv" });
    const pairs = [
      ["ID", n.id], ["Kind", n.kind], ["Region", n.region || "—"],
      ["Public URL", n.public_url], ["Version", n.version || "—"],
      ["Joined", new Date(n.joined_at).toLocaleString()],
      ["Uptime", fmtDur(n.uptime_seconds)],
      ["Routing score", n.score.toFixed(1) + " (lower is better)"],
      ["Sessions routed", n.routed_total.toLocaleString()],
      ["Cache", `${fmtBytes(Number(n.cache_bytes))} in ${n.cache_files} objects`],
    ];
    for (const [k, v] of pairs) kv.append(el("dt", { text: k }), el("dd", { text: String(v) }));
    metaCard.append(
      el("h2", null, el("span", null, stateChip(n)), el("span", { class: "h2-side" }, nodeActions(n, load))),
      kv,
    );

    const pts = hist.map(s => ({ ...s, ts: new Date(s.t).getTime() }));
    lineChart(hwBox, [
      { name: "CPU", points: pts.map(p => ({ t: p.ts, v: p.cpu })) },
      { name: "RAM", points: pts.map(p => ({ t: p.ts, v: p.ram })) },
      { name: "Disk", points: pts.map(p => ({ t: p.ts, v: p.disk })) },
    ], { max: 100, fmt: v => v.toFixed(0) + "%" });
    lineChart(netBox, [
      { name: "Egress", points: pts.map(p => ({ t: p.ts, v: p.out_rate })) },
      { name: "Ingest", points: pts.map(p => ({ t: p.ts, v: p.in_rate })) },
    ], { fmt: v => fmtBytes(v, "/s") });
    lineChart(connBox, [
      { name: "Connections", points: pts.map(p => ({ t: p.ts, v: p.conns })) },
    ], { fmt: v => v.toFixed(0), area: true, height: 140 });
  }

  await load().catch(e => { setConn(false); view.append(el("div", { class: "empty", text: "Node not found: " + e.message })); });
  setRefresh(load, 3000);
}

/* ----- enroll (tokens) ----- */

async function pageEnroll() {
  view.textContent = "";
  view.append(topline("Enroll nodes"));

  const createCard = el("div", { class: "card" }, el("h2", { text: "Mint a join token" }));
  const note = el("input", { placeholder: "what is this token for? (optional)" });
  const ttl = el("select", null,
    el("option", { value: "0", text: "never expires" }),
    el("option", { value: "3600", text: "expires in 1 hour" }),
    el("option", { value: "86400", text: "expires in 24 hours" }),
    el("option", { value: "604800", text: "expires in 7 days" }));
  const uses = el("select", null,
    el("option", { value: "0", text: "unlimited uses" }),
    el("option", { value: "1", text: "single use" }),
    el("option", { value: "5", text: "5 uses" }),
    el("option", { value: "20", text: "20 uses" }));
  const out = el("div");
  createCard.append(
    el("div", { class: "form-grid" },
      el("div", { class: "field" }, el("label", { text: "Note" }), note),
      el("div", { class: "field" }, el("label", { text: "Expiry" }), ttl),
      el("div", { class: "field" }, el("label", { text: "Use limit" }), uses),
    ),
    el("div", { class: "form-actions" },
      el("button", {
        class: "btn primary", text: "Create token",
        onclick: async () => {
          const info = await api.post("/api/v1/tokens", {
            note: note.value.trim(), ttl_seconds: +ttl.value, max_uses: +uses.value,
          });
          out.textContent = "";
          const edgeCmd = `hpcdn edge --controller-url ${location.origin} --join-token ${info.token}`;
          const originCmd = `hpcdn origin --controller-url ${location.origin} --join-token ${info.token} --watch-dir ./content`;
          out.append(
            el("p", { style: "margin:14px 0 8px;font-size:12.5px", text: "Token created — it is shown only once. Enroll nodes with:" }),
            el("div", { class: "snippet" }, copyBtn(edgeCmd), "# edge node\n" + edgeCmd),
            el("div", { style: "height:8px" }),
            el("div", { class: "snippet" }, copyBtn(originCmd), "# origin node\n" + originCmd),
          );
          loadTokens();
        },
      })),
    out,
  );

  const listCard = el("div", { class: "card" }, el("h2", { text: "Active join tokens" }));
  const listBox = el("div");
  listCard.append(listBox);
  view.append(createCard, listCard);

  async function loadTokens() {
    const toks = await api.get("/api/v1/tokens");
    setConn(true);
    listBox.textContent = "";
    if (!toks.length) {
      listBox.append(el("div", { class: "empty", text: "No join tokens. Create one above to enroll your first node." }));
      return;
    }
    const tbl = el("table");
    tbl.append(el("thead", null, el("tr", null,
      ...["ID", "Note", "Created", "Expires", "Uses", ""].map(h => el("th", { text: h })))));
    const tb = el("tbody");
    for (const t of toks) {
      tb.append(el("tr", null,
        el("td", null, el("span", { class: "td-id", text: t.id })),
        el("td", { text: t.note || "—" }),
        el("td", { text: new Date(t.created_at).toLocaleString() }),
        el("td", { text: t.expires_at ? new Date(t.expires_at).toLocaleString() : "never" }),
        el("td", { class: "num", text: t.max_uses ? `${t.uses}/${t.max_uses}` : String(t.uses) }),
        el("td", null, el("button", {
          class: "btn small danger", text: "revoke",
          onclick: async () => { await api.del(`/api/v1/tokens/${t.id}`); toast("Token revoked"); loadTokens(); },
        })),
      ));
    }
    tbl.append(tb);
    listBox.append(tbl);
  }

  await loadTokens().catch(() => setConn(false));
  setRefresh(null);
}

/* ----- settings ----- */

const SETTINGS_META = [
  ["cpu_weight", "CPU weight", "How strongly CPU saturation counts toward a node's routing score."],
  ["ram_weight", "RAM weight", "How strongly memory pressure counts toward the score."],
  ["conn_weight", "Connection weight", "How strongly connection-count vs capacity counts."],
  ["region_penalty", "Region penalty", "Score added to nodes outside the viewer's region."],
  ["saturation_score", "Saturation threshold", "Bounded-load limit: the affinity node is skipped above this score."],
  ["ewma_alpha", "EWMA alpha", "Telemetry smoothing (0–1). Higher reacts faster, lower is steadier."],
  ["heartbeat_ttl_seconds", "Heartbeat TTL (s)", "Node is offline when silent this long."],
  ["eject_after_missed", "Probe failures to eject", "Consecutive failed health probes before circuit-breaking a node."],
  ["panic_threshold", "Panic threshold", "If this fraction of the pool is ejected, ignore health and route anyway."],
  ["sign_ttl_seconds", "Signed URL TTL (s)", "Default lifetime of minted playback URLs."],
  ["affinity_enabled", "Cache affinity", "Route each stream to a consistent edge (rendezvous hashing)."],
];

async function pageSettings() {
  view.textContent = "";
  view.append(topline("Routing settings"));
  const card = el("div", { class: "card" },
    el("h2", null, "Live-tunable — applied to the next routing decision, no restart"));
  view.append(card);

  const s = await api.get("/api/v1/settings").catch(() => null);
  if (!s) { card.append(el("div", { class: "empty", text: "Could not load settings." })); return; }
  setConn(true);

  const inputs = {};
  const grid = el("div", { class: "form-grid" });
  for (const [key, label, hint] of SETTINGS_META) {
    let input;
    if (typeof s[key] === "boolean") {
      input = el("select", null,
        el("option", { value: "true", text: "enabled" }),
        el("option", { value: "false", text: "disabled" }));
      input.value = String(s[key]);
    } else {
      input = el("input", { type: "number", step: "any", value: String(s[key]) });
    }
    inputs[key] = input;
    grid.append(el("div", { class: "field" },
      el("label", { text: label }), input, el("span", { class: "hint", text: hint })));
  }
  const msg = el("span", { class: "form-msg" });
  card.append(grid, el("div", { class: "form-actions" },
    el("button", {
      class: "btn primary", text: "Apply settings",
      onclick: async () => {
        const body = { ...s };
        for (const [k, input] of Object.entries(inputs)) {
          body[k] = typeof s[k] === "boolean" ? input.value === "true" :
            Number.isInteger(s[k]) && !["cpu_weight","ram_weight","conn_weight","ewma_alpha","panic_threshold","region_penalty","saturation_score"].includes(k)
              ? parseInt(input.value, 10) : parseFloat(input.value);
        }
        try {
          await api.put("/api/v1/settings", body);
          msg.className = "form-msg"; msg.textContent = "Applied — routing uses these values now.";
        } catch (e) {
          msg.className = "form-msg err"; msg.textContent = e.message;
        }
      },
    }), msg));
  setRefresh(null);
}

/* ----- events & logs ----- */

async function pageEvents() {
  view.textContent = "";
  view.append(topline("Events & logs"));
  const feedCard = el("div", { class: "card" }, el("h2", null, "Cluster events ", el("span", { class: "h2-side", text: "live" })));
  const feed = el("div", { class: "feed" });
  feedCard.append(feed);
  const logCard = el("div", { class: "card" }, el("h2", { text: "Controller log (recent)" }));
  const logBox = el("div");
  logCard.append(logBox);
  view.append(feedCard, logCard);

  events.attach(feed);

  async function loadLogs() {
    const entries = await api.get("/api/v1/logs");
    setConn(true);
    logBox.textContent = "";
    for (const e of entries.slice(-120).reverse()) {
      const line = el("div", { class: "logline" });
      line.append(
        el("span", { class: "lg-time", text: new Date(e.time).toLocaleTimeString() + " " }),
        el("span", { class: "lvl-" + e.level, text: e.level.padEnd(5) + " " }),
        e.message + (e.attrs ? "  " + e.attrs : ""),
      );
      logBox.append(line);
    }
    if (!entries.length) logBox.append(el("div", { class: "empty", text: "No log entries yet." }));
  }
  await loadLogs().catch(() => setConn(false));
  setRefresh(loadLogs, 4000);
}

/* ---------- SSE over fetch (EventSource can't send Authorization) ---------- */

const events = {
  buf: [],
  target: null,
  started: false,

  attach(container) {
    this.target = container;
    this.render();
    this.start();
  },

  render() {
    if (!this.target || !this.target.isConnected) return;
    this.target.textContent = "";
    if (!this.buf.length) {
      this.target.append(el("div", { class: "empty", text: "Cluster events appear here — enrollments, ejections, purges." }));
      return;
    }
    for (const ev of [...this.buf].reverse().slice(0, 50)) {
      this.target.append(el("div", { class: "feed-row" },
        el("span", { class: "feed-time", text: new Date(ev.time).toLocaleTimeString([], { hour12: false }) }),
        el("span", { class: "feed-type", text: (ev.type || "").replaceAll("_", " ") }),
        el("span", { class: "feed-msg", text: ev.msg }),
      ));
    }
  },

  async start() {
    if (this.started) return;
    this.started = true;
    while (true) {
      try {
        const resp = await fetch("/api/v1/events", { headers: { Authorization: "Bearer " + api.key } });
        if (resp.status === 401) { this.started = false; return; }
        const reader = resp.body.getReader();
        const dec = new TextDecoder();
        let acc = "";
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          acc += dec.decode(value, { stream: true });
          let idx;
          while ((idx = acc.indexOf("\n\n")) >= 0) {
            const chunk = acc.slice(0, idx);
            acc = acc.slice(idx + 2);
            const dataLine = chunk.split("\n").find(l => l.startsWith("data: "));
            if (!dataLine) continue;
            try {
              const ev = JSON.parse(dataLine.slice(6));
              this.buf.push(ev);
              if (this.buf.length > 200) this.buf.shift();
              this.render();
            } catch { /* keepalive or partial */ }
          }
        }
      } catch { /* network blip */ }
      await new Promise(r => setTimeout(r, 3000));
    }
  },
};

/* ---------- router ---------- */

const routes = [
  { re: /^#?\/?$/, page: pageOverview, nav: "overview" },
  { re: /^#\/nodes$/, page: pageNodes, nav: "nodes" },
  { re: /^#\/nodes\/([^/]+)$/, page: m => pageNodeDetail(m[1]), nav: "nodes" },
  { re: /^#\/enroll$/, page: pageEnroll, nav: "enroll" },
  { re: /^#\/settings$/, page: pageSettings, nav: "settings" },
  { re: /^#\/events$/, page: pageEvents, nav: "events" },
];

function route() {
  const h = location.hash || "#/";
  for (const r of routes) {
    const m = h.match(r.re);
    if (m) {
      document.querySelectorAll(".nav a").forEach(a =>
        a.classList.toggle("active", a.dataset.nav === r.nav));
      Promise.resolve(r.page(m)).catch(e => {
        if (e.message !== "unauthorized") {
          view.textContent = "";
          view.append(el("div", { class: "empty", text: "Error: " + e.message }));
        }
      });
      return;
    }
  }
  location.hash = "#/";
}

/* ---------- boot ---------- */

initTheme();
$("#theme-toggle").addEventListener("click", () => {
  applyTheme(document.documentElement.getAttribute("data-theme") === "dark" ? "light" : "dark");
  route(); // re-render charts with new tokens
});
$("#gate-go").addEventListener("click", () => gate.submit());
$("#gate-key").addEventListener("keydown", e => { if (e.key === "Enter") gate.submit(); });
$("#logout").addEventListener("click", () => {
  localStorage.removeItem("hpcdn_key");
  api.key = "";
  gate.show();
});

window.addEventListener("hashchange", route);

(async () => {
  if (!api.key) { gate.show(); return; }
  try { await api.get("/api/v1/whoami"); gate.hide(); route(); }
  catch { gate.show(); }
})();
