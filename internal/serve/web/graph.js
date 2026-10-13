// graph.js — the stack as a graph: `runsnip graph --json` drawn on an SVG canvas. Loaded only when the Graph tab is
// opened. Nodes come in columns (ingress → edge → app → data → external); live state rides the stack's SSE stream.
// On a phone the same data is a vertical list grouped by layer.
const SVG = "http://www.w3.org/2000/svg";
const W = 196, H = 66, GX = 260, GY = 88; // node size, column and row pitch
const KINDS = { route: "routes", depends: "depends on", calls: "calls", data: "data" };
const LAYER_NAMES = { ingress: "Hostnames", edge: "Proxies", app: "Services", data: "Data", external: "Outside" };

function s(tag, attrs = {}, ...kids) {
  const e = document.createElementNS(SVG, tag);
  for (const [k, v] of Object.entries(attrs)) if (v != null) e.setAttribute(k, v);
  for (const k of kids) if (k != null) e.append(k);
  return e;
}

export async function render(ctx) {
  const { name, body, el, icon, api, act, pill, setLive } = ctx;
  body.replaceChildren(el("p", { class: "muted", text: "Reading the stack…" }));
  let g, saved;
  try { [g, saved] = await Promise.all([api("GET", `/api/stacks/${encodeURIComponent(name)}/graph`), api("GET", `/api/stacks/${encodeURIComponent(name)}/layout`)]); }
  catch (e) { body.replaceChildren(el("p", { class: "bad", text: e.message })); return; }
  if (g.accent && /^#[0-9a-f]{3,8}$/i.test(g.accent)) body.style.setProperty("--accent", g.accent);
  const byId = new Map(g.nodes.map((n) => [n.id, n]));
  const live = new Map(); // service → snapshot row
  const versions = new Map(); // component → version row
  const phone = matchMedia("(max-width: 720px)");

  // ── state of a node, from the last snapshot ──
  function stateOf(n) {
    if (n.off) return { v: "off", word: "off" };
    if (!n.service) return { v: "external", word: "" };
    const r = live.get(n.service);
    if (!r) return { v: "measuring", word: "…" };
    if (r.Off && !r.Container) return { v: "off", word: "off" };
    if (r.State !== "running") return { v: r.State || "exited", word: r.State || "not created" };
    if (r.Health === "unhealthy") return { v: "unhealthy", word: "unhealthy" };
    if (r.Health === "starting") return { v: "starting", word: "starting" };
    return { v: "healthy", word: r.Health || "running" };
  }
  const failing = (n) => ["exited", "dead", "unhealthy", "created", "not created", "restarting"].includes(stateOf(n).v);

  // ── phone: a list grouped by layer ──
  function listView() {
    const out = new Map();
    for (const e of g.edges) (out.get(e.from) || out.set(e.from, []).get(e.from)).push(e);
    const groups = g.layers.map((l) => [l, g.nodes.filter((n) => n.layer === l)]).filter(([, ns]) => ns.length);
    return el("div", { class: "glist" }, groups.map(([l, ns]) => el("section", { class: "panel" },
      el("h2", { text: LAYER_NAMES[l] || l }),
      ns.map((n) => {
        const st = stateOf(n);
        return el("div", { class: "gitem", "data-fail": String(failing(n)) },
          el("div", { class: "row" }, icon(n.icon), el("strong", { text: n.name }), n.service ? pill(st.v, st.word) : el("span", { class: "muted small", text: n.kind })),
          el("div", { class: "muted small", text: (out.get(n.id) || []).map((e) => `${KINDS[e.kind]} ${byId.get(e.to)?.name}`).join(" · ") }));
      }))));
  }

  // ── desktop: the canvas ──
  const pos = new Map(g.nodes.map((n) => [n.id, saved[n.id] ? { x: saved[n.id][0], y: saved[n.id][1] } : { x: 20 + n.col * GX, y: 20 + n.row * GY }]));
  const svg = s("svg", { class: "gcanvas", role: "img", "aria-label": `Graph of ${g.title}: ${g.nodes.length} nodes, ${g.edges.length} edges` });
  const defs = s("defs");
  for (const k of Object.keys(KINDS)) defs.append(s("marker", { id: "arrow-" + k, viewBox: "0 0 10 10", refX: "9", refY: "5", markerWidth: "7", markerHeight: "7", orient: "auto-start-reverse" }, s("path", { d: "M0,0 L10,5 L0,10 z", class: "ah-" + k })));
  svg.append(defs);
  const view = s("g");
  const edgeLayer = s("g", { class: "edges" }), nodeLayer = s("g", { class: "nodes" });
  view.append(edgeLayer, nodeLayer);
  svg.append(view);
  let tx = 0, ty = 0, k = 1;
  const apply = () => view.setAttribute("transform", `translate(${tx},${ty}) scale(${k})`);
  const edgeEls = [], nodeEls = new Map();

  function path(e) {
    const a = pos.get(e.from), b = pos.get(e.to);
    const x1 = a.x + W, y1 = a.y + H / 2, x2 = b.x, y2 = b.y + H / 2;
    const back = x2 < x1;
    const dx = Math.max(40, Math.abs(x2 - x1) / 2);
    return back ? `M${a.x + W / 2},${a.y + H} C${a.x + W / 2},${a.y + H + 60} ${b.x + W / 2},${b.y + H + 60} ${b.x + W / 2},${b.y + H}`
      : `M${x1},${y1} C${x1 + dx},${y1} ${x2 - dx},${y2} ${x2},${y2}`;
  }
  const direct = new Set(g.edges.filter((e) => e.kind === "calls" || e.kind === "data").map((e) => e.from + ">" + e.to));
  for (const e of g.edges) {
    if (e.kind === "depends" && direct.has(e.from + ">" + e.to)) continue; // the same arrow, already drawn as what it is
    const p = s("path", { class: "edge e-" + e.kind, "marker-end": `url(#arrow-${e.kind})` });
    const t = s("title"); t.textContent = `${byId.get(e.from).name} ${KINDS[e.kind]} ${byId.get(e.to).name}${e.labels ? ": " + e.labels.join(", ") : ""}${e.via ? " (" + e.via.join(", ") + ")" : ""}`;
    p.append(t);
    edgeLayer.append(p);
    edgeEls.push([e, p]);
  }
  for (const n of g.nodes) {
    const st = s("g", { class: "nstate" });
    const box = s("g", { class: "node k-" + n.kind, tabindex: "0", role: "button", "aria-label": n.name });
    box.append(s("rect", { width: W, height: H, rx: "10" }));
    const ic = icon(n.icon, "nicon"); ic.setAttribute("x", "10"); ic.setAttribute("y", "10"); ic.setAttribute("width", "22"); ic.setAttribute("height", "22");
    box.append(ic);
    const title = s("text", { x: "40", y: "22", class: "nname" }); title.textContent = n.name.length > 20 ? n.name.slice(0, 19) + "…" : n.name;
    const tt = s("title"); tt.textContent = n.name + (n.image ? " — " + n.image : "");
    box.append(tt);
    const sub = s("text", { x: "40", y: "39", class: "nsub" });
    const metr = s("text", { x: "40", y: "56", class: "nmetr" });
    box.append(title, sub, metr, st);
    nodeLayer.append(box);
    nodeEls.set(n.id, { box, sub, metr, st });
  }
  function place() {
    for (const n of g.nodes) nodeEls.get(n.id).box.setAttribute("transform", `translate(${pos.get(n.id).x},${pos.get(n.id).y})`);
    for (const [e, p] of edgeEls) p.setAttribute("d", path(e));
  }
  function paint() {
    const down = new Set(g.nodes.filter(failing).map((n) => n.id));
    for (const n of g.nodes) {
      const { box, sub, metr, st } = nodeEls.get(n.id);
      const state = stateOf(n);
      box.setAttribute("data-state", state.v);
      box.classList.toggle("down", down.has(n.id));
      const ver = n.component && versions.get(n.component);
      const subText = n.kind === "external" ? (n.hosts || []).slice(0, 1).join("") : ver ? (ver.Running || ver.Pinned || "") + (ver.Pinned && ver.Running && ver.Pinned !== ver.Running ? ` → ${ver.Pinned}` : "") : n.kind === "host" ? "hostname" : n.kind;
      sub.textContent = subText.length > 14 ? subText.slice(0, 13) + "…" : subText;
      const r = n.service && live.get(n.service);
      metr.textContent = r && r.State === "running" ? `CPU ${r.CPU || "–"} · RAM ${r.Mem ? r.Mem.split(" / ")[0] : "–"}` : "";
      st.replaceChildren();
      if (n.service) {
        // Shape and word, never colour alone: ● healthy, ▲ starting, ■ down, ○ off.
        const shape = { healthy: "M0,-4 a4,4 0 1,0 0.01,0", off: "M0,-4 a4,4 0 1,0 0.01,0", measuring: "M0,-4 L4,4 L-4,4 Z", starting: "M0,-4 L4,4 L-4,4 Z" }[state.v] || "M-4,-4 h8 v8 h-8 z";
        st.append(s("path", { d: shape, transform: `translate(${W - 14},35)`, class: "sh sh-" + state.v }));
        const w = s("text", { x: String(W - 22), y: "39", class: "nword", "text-anchor": "end" }); w.textContent = state.word;
        st.append(w);
      }
    }
    for (const [e, p] of edgeEls) p.classList.toggle("at-risk", down.has(e.to));
  }

  // ── hover: a node's upstream and downstream ──
  const outs = new Map(), ins = new Map();
  const push = (m, k, v) => { const l = m.get(k) || m.set(k, []).get(k); if (!l.includes(v)) l.push(v); };
  for (const e of g.edges) { push(outs, e.from, e.to); push(ins, e.to, e.from); }
  function reach(id, m) { const seen = new Set([id]), q = [id]; while (q.length) for (const x of m.get(q.shift()) || []) if (!seen.has(x)) { seen.add(x); q.push(x); } return seen; }
  function highlight(id) {
    svg.classList.toggle("focus", !!id);
    const on = id ? new Set([...reach(id, outs), ...reach(id, ins)]) : new Set();
    for (const n of g.nodes) nodeEls.get(n.id).box.classList.toggle("lit", on.has(n.id));
    for (const [e, p] of edgeEls) p.classList.toggle("lit", on.has(e.from) && on.has(e.to));
  }

  // ── pan, zoom, drag ──
  let drag = null, moved = false, saveT = 0;
  const pt = (ev) => { const r = svg.getBoundingClientRect(); return { x: (ev.clientX - r.left - tx) / k, y: (ev.clientY - r.top - ty) / k }; };
  svg.addEventListener("pointerdown", (ev) => {
    const box = ev.target.closest(".node");
    const id = box && [...nodeEls].find(([, v]) => v.box === box)?.[0];
    const p = pt(ev);
    drag = id ? { id, dx: p.x - pos.get(id).x, dy: p.y - pos.get(id).y } : { pan: true, x: ev.clientX - tx, y: ev.clientY - ty };
    moved = false;
    svg.setPointerCapture(ev.pointerId);
  });
  svg.addEventListener("pointermove", (ev) => {
    if (!drag) { const box = ev.target.closest?.(".node"); highlight(box ? [...nodeEls].find(([, v]) => v.box === box)?.[0] : null); return; }
    moved = true;
    if (drag.pan) { tx = ev.clientX - drag.x; ty = ev.clientY - drag.y; apply(); return; }
    const p = pt(ev);
    pos.set(drag.id, { x: Math.round(p.x - drag.dx), y: Math.round(p.y - drag.dy) });
    place();
  });
  svg.addEventListener("pointerup", () => {
    if (drag && drag.id && !moved) open(drag.id);
    if (drag && drag.id && moved) { clearTimeout(saveT); saveT = setTimeout(save, 600); }
    drag = null;
  });
  svg.addEventListener("pointerleave", () => highlight(null));
  svg.addEventListener("wheel", (ev) => {
    ev.preventDefault();
    const r = svg.getBoundingClientRect(), mx = ev.clientX - r.left, my = ev.clientY - r.top;
    const nk = Math.min(2.5, Math.max(0.25, k * (ev.deltaY < 0 ? 1.1 : 0.9)));
    tx = mx - (mx - tx) * nk / k; ty = my - (my - ty) * nk / k; k = nk; apply();
  }, { passive: false });
  svg.addEventListener("keydown", (ev) => { if (ev.key === "Enter") { const box = ev.target.closest(".node"); const id = box && [...nodeEls].find(([, v]) => v.box === box)?.[0]; if (id) open(id); } });
  async function save() {
    const o = {};
    for (const [id, p] of pos) o[id] = [p.x, p.y];
    try { await api("PUT", `/api/stacks/${encodeURIComponent(name)}/layout`, o); } catch { /* the next drag tries again */ }
  }
  function fit() {
    const r = svg.getBoundingClientRect();
    const xs = [...pos.values()].map((p) => p.x), ys = [...pos.values()].map((p) => p.y);
    const w = Math.max(...xs) + W + 40 - Math.min(...xs), h = Math.max(...ys) + H + 80 - Math.min(...ys);
    k = Math.min(1.2, Math.max(0.25, Math.min(r.width / w, r.height / h)));
    tx = -Math.min(...xs) * k + 10; ty = -Math.min(...ys) * k + 10; apply();
  }

  // ── the side panel ──
  const side = el("aside", { class: "gside panel", hidden: true });
  function open(id) {
    const n = byId.get(id);
    const r = n.service && live.get(n.service);
    const ver = n.component && versions.get(n.component);
    const slot = el("div");
    const input = el("input", { placeholder: "version", size: "8", "aria-label": `new version of ${n.component}` });
    const list = (m, word) => (m.get(id) || []).map((x) => el("li", {}, `${word} `, el("button", { type: "button", class: "link", text: byId.get(x).name, onclick: () => open(x) })));
    const logs = el("pre", { class: "log", hidden: true });
    let es = null;
    side.replaceChildren(...[
      el("div", { class: "row spread" }, el("h3", {}, icon(n.icon), n.name), el("button", { type: "button", class: "link", icon: "close", text: "Close", onclick: () => { es && es.close(); side.hidden = true; } })),
      el("dl", {},
        el("dt", { text: "Kind" }), el("dd", { text: `${n.kind} · ${LAYER_NAMES[n.layer] || n.layer}` }),
        n.image ? [el("dt", { text: "Image" }), el("dd", { class: "mono", text: n.image })] : null,
        ver ? [el("dt", { text: "Version" }), el("dd", { text: `${ver.Running || "–"} (pinned ${ver.Pinned || "–"}${ver.Previous ? ", before " + ver.Previous : ""})` })] : null,
        n.service ? [el("dt", { text: "State" }), el("dd", {}, pill(stateOf(n).v, stateOf(n).word), r && r.Status ? " " + r.Status : "")] : null,
        n.hosts ? [el("dt", { text: "Hosts" }), el("dd", { class: "mono", text: n.hosts.join(" ") })] : null,
        n.networks ? [el("dt", { text: "Networks" }), el("dd", { text: n.networks.join(", ") })] : null,
        n.off ? [el("dt", { text: "Off" }), el("dd", { text: n.off })] : null),
      el("ul", { class: "gedges" }, list(outs, "→"), list(ins, "←")),
      n.service && !n.off ? el("div", { class: "row" },
        el("button", { type: "button", icon: "restart", text: "Restart", onclick: () => act(slot, name, ["restart", n.service, "--no-pull"], `Recreate ${n.service} from its current image?`) }),
        el("button", { type: "button", icon: "logs", text: "Logs", onclick: () => {
          if (es) { es.close(); es = null; logs.hidden = true; return; }
          logs.hidden = false; logs.textContent = "";
          es = new EventSource(`/api/stacks/${encodeURIComponent(name)}/logs?service=${encodeURIComponent(n.service)}`);
          setLive(es);
          es.onmessage = (m) => { logs.append(JSON.parse(m.data) + "\n"); while (logs.childNodes.length > 1000) logs.firstChild.remove(); logs.scrollTop = logs.scrollHeight; };
        } })) : null,
      n.component ? el("div", { class: "row" }, input,
        el("button", { type: "button", icon: "pin", text: "Pin", onclick: () => input.value && act(slot, name, ["pin", n.component, input.value.trim()], `Pin ${n.component} to ${input.value}, then pull and recreate it?`) }),
        ver && ver.Previous ? el("button", { type: "button", icon: "rollback", text: `Back to ${ver.Previous}`, onclick: () => act(slot, name, ["rollback", n.component], `Roll ${n.component} back to ${ver.Previous}?`) }) : null) : null,
      logs, slot].filter((x) => x != null));
    side.hidden = false;
  }

  // ── chrome: toolbar, legend ──
  const nets = el("input", { type: "checkbox", id: "gnets" });
  const legend = el("div", { class: "glegend" },
    Object.entries(KINDS).map(([kk, w]) => {
      const cb = el("input", { type: "checkbox", "aria-label": `show ${w} edges` });
      cb.checked = true;
      cb.addEventListener("change", () => svg.classList.toggle("hide-" + kk, !cb.checked));
      return el("label", { class: "lg inline" }, cb, s("svg", { width: "28", height: "10", "aria-hidden": "true" }, s("path", { d: "M2,5 L26,5", class: "edge e-" + kk })), w);
    }),
    el("span", { class: "lg" }, "● healthy ▲ starting ■ down ○ off"));
  const netBox = el("div", { class: "gnets", hidden: true }, g.networks.map((nw, i) => el("span", { class: "chip n" + (i % 6) }, `${nw.name}: ${nw.members.length}`)));
  nets.addEventListener("change", () => {
    netBox.hidden = !nets.checked;
    const idx = new Map(g.networks.map((nw, i) => [nw.name, i % 6]));
    for (const n of g.nodes) {
      const { box } = nodeEls.get(n.id);
      box.querySelectorAll(".ndot").forEach((d) => d.remove());
      if (nets.checked) (n.networks || []).forEach((nw, j) => box.append(s("circle", { cx: String(12 + j * 9), cy: String(H - 8), r: "3.5", class: "ndot n" + idx.get(nw) })));
    }
  });
  const toolbar = el("div", { class: "row gtools" },
    el("button", { type: "button", icon: "zoom-in", text: "Zoom in", onclick: () => { k = Math.min(2.5, k * 1.2); apply(); } }),
    el("button", { type: "button", icon: "zoom-out", text: "Zoom out", onclick: () => { k = Math.max(0.25, k / 1.2); apply(); } }),
    el("button", { type: "button", icon: "fit", text: "Fit", onclick: fit }),
    el("button", { type: "button", icon: "reset", text: "Reset layout", onclick: async () => {
      try { await api("DELETE", `/api/stacks/${encodeURIComponent(name)}/layout`); } catch { /* still reset here */ }
      for (const n of g.nodes) pos.set(n.id, { x: 20 + n.col * GX, y: 20 + n.row * GY });
      place(); fit();
    } }),
    el("label", { class: "inline" }, nets, "networks"));

  const desk = el("div", { class: "gwrap" }, toolbar, legend, netBox, el("div", { class: "gstage" }, svg, side));
  const mob = el("div");
  function mode() {
    if (phone.matches) { mob.replaceChildren(listView()); desk.hidden = true; mob.hidden = false; }
    else { desk.hidden = false; mob.hidden = true; }
  }
  body.replaceChildren(desk, mob);
  place(); paint(); mode();
  requestAnimationFrame(fit);
  phone.addEventListener("change", mode);

  // ── live: the stack's own snapshot stream ──
  const es = new EventSource(`/api/stacks/${encodeURIComponent(name)}/events`);
  setLive(es);
  es.onmessage = (m) => {
    const snap = JSON.parse(m.data);
    live.clear(); versions.clear();
    for (const r of snap.services || []) live.set(r.Name, r);
    for (const v of snap.versions || []) versions.set(v.Component, v);
    paint();
    if (phone.matches) mob.replaceChildren(listView());
  };
}
