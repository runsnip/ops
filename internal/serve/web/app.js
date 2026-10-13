// runsnip serve — the page. No framework and no third-party request. It never does work itself: every change is a
// runsnip command the server starts as a job, whose log streams here. Live numbers come over Server-Sent Events only
// while a stack's page is open (the server samples nothing otherwise).
"use strict";

const $ = (s, root = document) => root.querySelector(s);
const main = () => $("#main");
const words = { normal: "✓ normal", warning: "● warning", critical: "▲ critical", measuring: "… measuring" };
let me = { csrf: "", name: "", version: "" };

// ───────────────────────── helpers ─────────────────────────

const SVG = "http://www.w3.org/2000/svg";
// icon is one icon of the sprite (icons.svg, from @runsnip/icons): decorative, the word beside it says the same.
function icon(name, cls = "icon") {
  const s = document.createElementNS(SVG, "svg");
  s.setAttribute("class", cls); s.setAttribute("aria-hidden", "true");
  const u = document.createElementNS(SVG, "use");
  u.setAttribute("href", "#i-" + name);
  s.append(u);
  return s;
}
async function loadIcons() {
  try {
    const t = await (await fetch("/icons.svg")).text();
    const box = document.createElement("div");
    box.hidden = true; box.innerHTML = t; // our own file, same origin
    document.body.prepend(box);
  } catch { /* words stay */ }
}
const stateIcon = (v) => ({ running: "ok", healthy: "ok", ok: "ok", normal: "ok", starting: "measuring", restarting: "measuring",
  measuring: "measuring", warning: "warn", unhealthy: "fail", exited: "fail", dead: "fail", failed: "fail", critical: "fail", lost: "unknown", off: "off", created: "fail", "not created": "fail", paused: "warn" })[v] || "unknown";
function pill(v, text) { return el("span", { class: "pill", "data-v": v }, icon(stateIcon(v), "icon sm"), text ?? v); }

function el(tag, attrs = {}, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === "icon") e.append(icon(v));
    else if (k === "text") e.append(v);
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else e.setAttribute(k, v === true ? "" : v);
  }
  for (const k of kids.flat()) if (k != null && k !== false) e.append(k);
  return e;
}
const store = {
  get(k) { try { return sessionStorage.getItem(k); } catch { return null; } },
  set(k, v) { try { v == null ? sessionStorage.removeItem(k) : sessionStorage.setItem(k, v); } catch { /* private window */ } },
};
function page(title, ...kids) { document.title = title ? `${title} · runsnip` : "runsnip"; main().replaceChildren(...kids.flat().filter((k) => k != null && k !== false)); }
function errorBox(e) { return el("p", { class: "bad", role: "alert", text: e.message || String(e) }); }
function when(t) { if (!t) return "–"; const d = new Date(t); return d.toLocaleString(); }
function ago(t) {
  const s = Math.round((Date.now() - new Date(t)) / 1000);
  return s < 60 ? `${s}s ago` : s < 3600 ? `${Math.round(s / 60)} min ago` : s < 86400 ? `${Math.round(s / 3600)} h ago` : when(t);
}

class APIError extends Error { constructor(status, msg) { super(msg); this.status = status; } }

// api calls the server; a 428 asks for a fresh authentication, then retries once.
async function api(method, path, body, retried = false) {
  const r = await fetch(path, {
    method, credentials: "same-origin",
    headers: { "Content-Type": "application/json", "X-CSRF-Token": me.csrf },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const j = await r.json().catch(() => ({}));
  if (r.status === 428 && !retried) { await reauth(); return api(method, path, body, true); }
  if (r.status === 401 && path !== "/api/status" && !path.startsWith("/api/login") && !path.startsWith("/api/setup") && !path.startsWith("/api/enroll")) { boot(); throw new APIError(401, "signed out"); }
  if (!r.ok) throw new APIError(r.status, j.error || r.statusText);
  return j;
}

// ───────────────────────── passkeys ─────────────────────────

const b64u = {
  dec(s) { s = s.replace(/-/g, "+").replace(/_/g, "/"); while (s.length % 4) s += "="; return Uint8Array.from(atob(s), (c) => c.charCodeAt(0)).buffer; },
  enc(b) { let s = ""; for (const x of new Uint8Array(b)) s += String.fromCharCode(x); return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, ""); },
};
function creationOptions(o) {
  const p = { ...o.publicKey };
  p.challenge = b64u.dec(p.challenge); p.user = { ...p.user, id: b64u.dec(p.user.id) };
  p.excludeCredentials = (p.excludeCredentials || []).map((c) => ({ ...c, id: b64u.dec(c.id) }));
  return { publicKey: p };
}
function requestOptions(o) {
  const p = { ...o.publicKey };
  p.challenge = b64u.dec(p.challenge);
  p.allowCredentials = (p.allowCredentials || []).map((c) => ({ ...c, id: b64u.dec(c.id) }));
  return { publicKey: p };
}
function credentialJSON(c) {
  const r = c.response, out = { id: c.id, rawId: b64u.enc(c.rawId), type: c.type, clientExtensionResults: c.getClientExtensionResults(), response: { clientDataJSON: b64u.enc(r.clientDataJSON) } };
  if (r.attestationObject) {
    out.response.attestationObject = b64u.enc(r.attestationObject);
    if (r.getTransports) out.response.transports = r.getTransports();
  } else {
    out.response.authenticatorData = b64u.enc(r.authenticatorData);
    out.response.signature = b64u.enc(r.signature);
    if (r.userHandle) out.response.userHandle = b64u.enc(r.userHandle);
  }
  return out;
}
const hasPasskeys = () => !!window.PublicKeyCredential;
async function passkeyRegister() {
  const b = await api("POST", "/api/passkey/register/begin");
  const c = await navigator.credentials.create(creationOptions(b.options));
  return api("POST", "/api/passkey/register/finish", { ceremony: b.ceremony, credential: credentialJSON(c) });
}
async function passkeyAssert(prefix) {
  const b = await api("POST", `${prefix}/begin`);
  const c = await navigator.credentials.get(requestOptions(b.options));
  return api("POST", `${prefix}/finish`, { ceremony: b.ceremony, credential: credentialJSON(c) });
}

// reauth asks for the passkey or a TOTP code; it resolves once the session is fresh again.
function reauth() {
  const d = $("#reauth"), err = $("#reautherr"), code = $("#reauthcode");
  err.textContent = ""; code.value = "";
  $("#reauthpk").hidden = !hasPasskeys();
  return new Promise((resolve, reject) => {
    const done = (ok) => { cleanup(); d.close(); ok ? resolve() : reject(new Error("not confirmed")); };
    const pk = async () => { try { await passkeyAssert("/api/reauth/passkey"); done(true); } catch (e) { err.textContent = e.message; } };
    const totp = async () => { try { await api("POST", "/api/reauth/totp", { code: code.value.trim() }, true); done(true); } catch (e) { err.textContent = e.message; } };
    const cancel = () => done(false);
    const cleanup = () => { $("#reauthpk").removeEventListener("click", pk); $("#reauthtotp").removeEventListener("click", totp); d.removeEventListener("cancel", cancel); $("#reauthform").removeEventListener("submit", cancel); };
    $("#reauthpk").addEventListener("click", pk); $("#reauthtotp").addEventListener("click", totp);
    d.addEventListener("cancel", cancel); $("#reauthform").addEventListener("submit", cancel);
    d.showModal();
  });
}

// ask is a confirmation in the page's own dialog (a native confirm() blocks the page and cannot be styled).
function ask(question, okLabel = "Run") {
  const d = $("#ask");
  $("#asktext").textContent = question;
  $("#askok").textContent = okLabel;
  return new Promise((resolve) => {
    d.addEventListener("close", () => resolve(d.returnValue === "ok"), { once: true });
    d.returnValue = "";
    d.showModal();
  });
}

// ───────────────────────── jobs ─────────────────────────

// runJob starts `runsnip [--stack s] command…` and returns the job record.
async function runJob(stack, command, stdin) {
  return api("POST", "/api/jobs", { stack: stack || "", command, stdin: stdin || "" });
}

// jobView streams a job's log into a panel; onEnd gets the finished job. It returns the panel and a promise of the end.
function jobView(job, opts = {}) {
  const pre = el("pre", { class: "log", "aria-live": "polite" });
  const head = el("div", { class: "row" }, el("strong", { text: `runsnip ${job.stack && !job.stack.startsWith("_") ? "--stack " + job.stack + " " : ""}${job.command.join(" ")}` }), pill("running"));
  head.insertBefore(el("button", { type: "button", class: "link", icon: "copy", text: "Copy", title: "copy the log", onclick: () => navigator.clipboard?.writeText(pre.textContent) }), head.lastChild);
  const panel = el("section", { class: "panel job" }, head, pre);
  const ended = new Promise((resolve) => {
    let lines = 0;
    const es = new EventSource(`/api/jobs/${encodeURIComponent(job.id)}/log`);
    es.onmessage = (m) => {
      const t = JSON.parse(m.data); pre.append(t + "\n"); lines++;
      if (lines > 5000) pre.firstChild && pre.firstChild.remove();
      pre.scrollTop = pre.scrollHeight;
    };
    es.addEventListener("end", (m) => {
      es.close();
      const j = JSON.parse(m.data);
      head.lastChild.replaceWith(pill(j.status, `${j.status}${j.exit != null ? " (exit " + j.exit + ")" : ""}`));
      resolve(j);
    });
    es.onerror = () => { if (opts.onDrop) { es.close(); opts.onDrop(); } };
  });
  return { panel, ended, pre };
}

// act confirms, runs the job and shows its log in a slot.
async function act(slot, stack, command, question, stdin) {
  if (question && !(await ask(question))) return null;
  try {
    const j = await runJob(stack, command, stdin);
    const v = jobView(j);
    slot.replaceChildren(v.panel);
    v.panel.scrollIntoView({ block: "nearest" });
    return v.ended;
  } catch (e) { slot.replaceChildren(errorBox(e)); return null; }
}

// ───────────────────────── getting in ─────────────────────────

function setupPage() {
  const code = el("input", { required: true, autocomplete: "off", placeholder: "XXXXX-XXXXX", "aria-label": "setup code" });
  const name = el("input", { required: true, autocomplete: "username", placeholder: "your name", "aria-label": "admin name", pattern: "[a-z0-9][a-z0-9._-]{0,31}" });
  const out = el("div");
  const f = el("form", { class: "panel narrow", onsubmit: async (e) => {
    e.preventDefault(); out.replaceChildren();
    try { const r = await api("POST", "/api/setup", { code: code.value.trim(), name: name.value.trim() }); me.csrf = r.csrf; me.name = r.name; enrolPage(); } catch (err) { out.replaceChildren(errorBox(err)); }
  } },
  el("h2", { text: "Create the first admin" }),
  el("p", { class: "muted", text: "On the server, run sudo runsnip admin setup-code and type the code it prints. It works once, for a short time, and only until the first admin exists." }),
  el("label", {}, "Setup code", code), el("label", {}, "Your admin name", name),
  el("div", { class: "row" }, el("button", { class: "primary", text: "Continue" })), out);
  page("Setup", f);
}

function enrolPage() {
  const out = el("div");
  const totpBox = el("div");
  const pk = el("button", { type: "button", class: "primary", text: "Register a passkey", onclick: async () => {
    out.replaceChildren();
    try { await passkeyRegister(); boot(); } catch (e) { out.replaceChildren(errorBox(e)); }
  } });
  const totp = el("button", { type: "button", text: "Use a TOTP app instead", onclick: async () => {
    try {
      const b = await api("POST", "/api/totp/begin");
      const code = el("input", { inputmode: "numeric", autocomplete: "one-time-code", maxlength: "6", placeholder: "123456", "aria-label": "code from the app" });
      totpBox.replaceChildren(
        el("p", {}, "Add this key to your authenticator app (or open ", el("a", { href: b.uri, text: "the otpauth link" }), " on your phone):"),
        el("p", { class: "mono big-secret", text: b.secret }),
        el("div", { class: "row" }, code, el("button", { type: "button", class: "primary", text: "Confirm", onclick: async () => {
          try { await api("POST", "/api/totp/confirm", { code: code.value.trim() }); boot(); } catch (e) { out.replaceChildren(errorBox(e)); }
        } })));
    } catch (e) { out.replaceChildren(errorBox(e)); }
  } });
  if (!hasPasskeys()) pk.disabled = true;
  page("Sign-in method", el("section", { class: "panel narrow" },
    el("h2", { text: `Welcome, ${me.name}` }),
    el("p", { class: "muted", text: "Choose how you sign in. A passkey (Face ID, Touch ID, Windows Hello, a security key) is the safest; a TOTP app is the fallback." }),
    el("div", { class: "row" }, pk, totp), totpBox, out));
}

function loginPage() {
  const out = el("div");
  const name = el("input", { autocomplete: "username", placeholder: "admin name", "aria-label": "admin name" });
  const code = el("input", { inputmode: "numeric", autocomplete: "one-time-code", maxlength: "6", placeholder: "123456", "aria-label": "TOTP code" });
  const ename = el("input", { autocomplete: "username", placeholder: "admin name", "aria-label": "admin name for the enrolment code" });
  const ecode = el("input", { autocomplete: "off", placeholder: "XXXXX-XXXXX", "aria-label": "enrolment code" });
  const signedIn = (r) => { me.csrf = r.csrf; me.name = r.name; boot(); };
  page("Sign in", el("section", { class: "panel narrow" },
    el("h2", { text: "Sign in" }),
    el("div", { class: "row" }, el("button", { type: "button", class: "primary", disabled: !hasPasskeys(), text: "Sign in with a passkey", onclick: async () => {
      out.replaceChildren(); try { signedIn(await passkeyAssert("/api/login/passkey")); } catch (e) { out.replaceChildren(errorBox(e)); }
    } })),
    el("form", { onsubmit: async (e) => { e.preventDefault(); out.replaceChildren(); try { signedIn(await api("POST", "/api/login/totp", { name: name.value.trim(), code: code.value.trim() })); } catch (err) { out.replaceChildren(errorBox(err)); } } },
      el("h3", { text: "Or with a TOTP code" }), el("div", { class: "row" }, name, code, el("button", { text: "Sign in" }))),
    el("details", {}, el("summary", { text: "I have an enrolment code" }),
      el("p", { class: "muted", text: "An admin made one for you with runsnip admin add or reset." }),
      el("form", { onsubmit: async (e) => { e.preventDefault(); out.replaceChildren(); try { const r = await api("POST", "/api/enroll", { name: ename.value.trim(), code: ecode.value.trim() }); me.csrf = r.csrf; me.name = r.name; enrolPage(); } catch (err) { out.replaceChildren(errorBox(err)); } } },
        el("div", { class: "row" }, ename, ecode, el("button", { text: "Continue" })))),
    out));
}

// ───────────────────────── stacks ─────────────────────────

async function stacksPage() {
  page("Stacks", el("p", { class: "muted", text: "Loading…" }));
  let list;
  try { list = await api("GET", "/api/stacks"); } catch (e) { return page("Stacks", errorBox(e)); }
  page("Stacks",
    el("div", { class: "row spread" }, el("h2", { text: "Stacks on this server" }), el("a", { class: "button primary", href: "#/add", text: "Add a stack" })),
    list.length ? el("div", { class: "grid" }, list.map((s) => el("a", { class: "panel stack", href: `#/s/${encodeURIComponent(s.name)}` },
      el("h3", { text: s.title || s.name }), el("div", { class: "muted mono", text: s.name }),
      s.error ? el("p", { class: "bad", text: s.error }) : null,
      el("dl", {}, el("dt", { text: "Checkout" }), el("dd", { class: "mono", text: s.dir }), el("dt", { text: "Data" }), el("dd", { class: "mono", text: s.data || "–" }),
        s.repo ? [el("dt", { text: "Infra" }), el("dd", { class: "mono", text: `${s.repo} ${s.branch || ""}` })] : null))))
      : el("section", { class: "panel" }, el("p", { text: "No stack yet." }), el("p", { class: "muted", text: "Add one from its infra repository: runsnip clones it, reads stack.yaml, walks you through .env and deploys it." })));
}

function addPage() {
  const f = {};
  const field = (key, label, ph, hint) => {
    f[key] = el("input", { name: key, placeholder: ph, autocomplete: "off", spellcheck: "false", "aria-label": label });
    return el("label", {}, label, f[key], hint ? el("small", { class: "muted", text: hint }) : null);
  };
  const checks = el("div", { class: "checks" });
  const keyBox = el("div");
  const slot = el("div");
  let timer = 0, okPaths = false;
  const check = async () => {
    const name = f.name.value.trim(), dir = f.dir.value.trim(), data = f.data.value.trim();
    okPaths = false;
    if (!name || !dir || !data) { checks.replaceChildren(el("p", { class: "muted", text: "Type a name and both paths: runsnip checks them as you type." })); return; }
    try {
      const rs = await api("POST", "/api/paths/check", { name, dir, data });
      okPaths = rs.length > 0 && rs.every((r) => r.ok);
      checks.replaceChildren(...rs.map((r) => el("div", { class: "check", "data-ok": String(r.ok) },
        el("strong", { text: `${r.ok ? "✓" : "✗"} ${r.kind}: ` }), el("span", { class: "mono", text: r.resolved && r.resolved !== r.path ? `${r.path} → ${r.resolved}` : r.path }),
        r.freeGB ? el("span", { class: "muted", text: ` · ${r.freeGB.toFixed(1)} GB free` }) : null,
        el("ul", {}, (r.reasons || []).map((x) => el("li", { class: "bad", text: x })), (r.warnings || []).map((x) => el("li", { class: "warn", text: x }))))));
    } catch (e) { checks.replaceChildren(errorBox(e)); }
  };
  const later = () => { clearTimeout(timer); timer = setTimeout(check, 350); };
  const form = el("form", { class: "panel", onsubmit: async (e) => {
    e.preventDefault();
    await check();
    if (!okPaths) return;
    const name = f.name.value.trim();
    const cmd = ["stack", "add", name, "--repo", f.repo.value.trim(), "--dir", f.dir.value.trim(), "--data", f.data.value.trim()];
    if (f.branch.value.trim()) cmd.push("--branch", f.branch.value.trim());
    const j = await act(slot, "", cmd);
    if (j && j.status === "ok") {
      store.set("wizard:" + name, "1");
      slot.append(el("div", { class: "row" }, el("a", { class: "button primary", href: `#/s/${encodeURIComponent(name)}/env`, text: "Next: fill in .env" })));
    }
  } },
  el("h2", { text: "Add a stack" }),
  el("p", { class: "muted", text: "You choose where it lives: nothing is assumed. runsnip refuses paths that are not absolute, system folders, folders of another stack, a checkout that is not empty, data that belongs to something else, or memory-backed filesystems — each with its reason." }),
  field("name", "Name", "e.g. shop", "lowercase letters, digits, - and _: --stack uses it"),
  field("repo", "Infra repository", "e.g. git@github.com:owner/repo.git", "SSH for a private repository (add a deploy key below), or https"),
  field("branch", "Branch", "e.g. infra", "the branch holding stack.yaml and the compose file"),
  field("dir", "Checkout path", "e.g. /opt/<name>", "absolute; must be empty or absent"),
  field("data", "Data path", "e.g. /srv/<name>", "absolute; empty, absent or already this stack's"),
  checks,
  el("details", {}, el("summary", { text: "Private repository: deploy key" }),
    el("p", { class: "muted", text: "runsnip makes an SSH key for this stack only (kept in its state folder, mode 600). Add the public key to the repository as a read-only deploy key, then add the stack." }),
    el("button", { type: "button", text: "Make the deploy key", onclick: () => { const n = f.name.value.trim(); if (n) act(keyBox, "", ["stack", "keygen", n]); } }), keyBox),
  el("div", { class: "row" }, el("button", { class: "primary", text: "Check, clone and register" })), slot);
  for (const k of Object.keys(f)) f[k].addEventListener("input", later);
  page("Add a stack", form);
  check();
}

// ───────────────────────── one stack ─────────────────────────

const cardIcon = (l) => /cpu/i.test(l) ? "cpu" : /ram|mem/i.test(l) ? "ram" : /net/i.test(l) ? "network" : /health/i.test(l) ? "healthy" : "overview";
let live = null; // the open EventSource(s) of a stack page
function stopLive() { if (live) { live.forEach((x) => x.close()); live = null; } }

function spark(hist) {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", "0 0 100 30"); svg.setAttribute("preserveAspectRatio", "none"); svg.setAttribute("aria-hidden", "true");
  const h = hist || [];
  if (h.length > 1) {
    const top = Math.max(...h, 1e-9);
    const pl = document.createElementNS("http://www.w3.org/2000/svg", "polyline");
    pl.setAttribute("points", h.map((v, i) => `${(i / (h.length - 1)) * 100},${29 - (v / top) * 27}`).join(" "));
    svg.append(pl);
  }
  return svg;
}
function tagOf(image) { if (!image) return "–"; const i = image.lastIndexOf(":"); return i > image.lastIndexOf("/") ? image.slice(i + 1) : "latest"; }

async function stackPage(name, tab) {
  stopLive();
  const tabs = [["", "Overview", "overview"], ["graph", "Graph", "graph"], ["logs", "Logs", "logs"], ["env", ".env", "env"], ["jobs", "Jobs", "jobs"]];
  const slot = el("div");
  const body = el("div");
  const actions = el("div", { class: "row" },
    el("button", { type: "button", class: "primary", icon: "deploy", text: "Deploy", onclick: () => act(slot, name, ["start"], `Deploy ${name}: pull the infra, pull images and start every enabled service?`) }),
    el("button", { type: "button", icon: "doctor", text: "Doctor", onclick: () => act(slot, name, ["doctor"]) }),
    el("button", { type: "button", icon: "updates", text: "Pull infra", onclick: () => act(slot, name, ["infra", "pull"]) }),
    el("button", { type: "button", icon: "lock", text: "Registry login", onclick: () => act(slot, name, ["login"]) }),
    el("button", { type: "button", icon: "backup", text: "Backup", onclick: () => act(slot, name, ["backup"]) }),
    el("button", { type: "button", icon: "restart", text: "Reload proxies", onclick: () => act(slot, name, ["reload"]) }));
  const wizard = store.get("wizard:" + name) ? el("section", { class: "panel wizard" },
    el("h3", { text: `Setting up ${name}` }),
    el("ol", {}, el("li", {}, el("a", { href: `#/s/${encodeURIComponent(name)}/env`, text: "Fill in .env" }), " — the ‘Missing’ filter shows what still needs a value; ‘Generate’ makes secrets."),
      el("li", { text: "Registry login, if the images are private." }), el("li", { text: "Deploy, and watch the services turn healthy below." })),
    el("button", { type: "button", class: "link", text: "Done, hide this", onclick: (e) => { store.set("wizard:" + name, null); e.target.closest("section").remove(); } })) : null;
  page(name,
    el("div", { class: "row spread" }, el("h2", { id: "stacktitle", text: name }), actions),
    wizard,
    el("nav", { class: "tabs" }, tabs.map(([k, l, ic]) => el("a", { href: `#/s/${encodeURIComponent(name)}${k ? "/" + k : ""}`, "aria-current": (tab || "") === k ? "page" : null, icon: ic, text: l }))),
    slot, body);
  if (!tab) overview(name, body, slot);
  else if (tab === "logs") logsTab(name, body);
  else if (tab === "env") envTab(name, body, slot);
  else if (tab === "jobs") jobsList(body, name);
  else if (tab === "graph") import("/graph.js").then((m) => m.render({ name, body, slot, el, icon, api, act, ask, pill, stateIcon, setLive: (x) => { (live = live || []).push(x); } }))
    .catch((e) => body.replaceChildren(errorBox(e)));
}

function overview(name, body, slot) {
  const cards = el("section", { class: "cards", "aria-label": "At a glance" });
  const err = el("p", { class: "bad", hidden: true });
  const svc = el("tbody"), ver = el("tbody"), sys = el("dl"), sh = el("div");
  const shieldBox = el("div", { hidden: true }, el("h2", { text: "Request shield" }), sh);
  body.replaceChildren(cards, err,
    el("section", { class: "panel" }, el("h2", { text: "Services" }), el("div", { class: "scroll" }, el("table", {}, el("thead", {}, el("tr", {}, ["Service", "State", "Health", "Tag", "CPU", "Memory", "Status", ""].map((h) => el("th", { text: h })))), svc))),
    el("section", { class: "panel" }, el("h2", { text: "Versions" }), el("div", { class: "scroll" }, el("table", {}, el("thead", {}, el("tr", {}, ["Component", "Pinned", "Running", "Previous", "", ""].map((h) => el("th", { text: h })))), ver))),
    el("section", { class: "panel two" }, el("div", {}, el("h2", { text: "System" }), sys), shieldBox));
  const es = new EventSource(`/api/stacks/${encodeURIComponent(name)}/events`);
  live = [es];
  es.onmessage = (m) => {
    const s = JSON.parse(m.data);
    $("#stacktitle") && ($("#stacktitle").textContent = s.title || name);
    err.hidden = !s.error; err.textContent = s.error || "";
    cards.replaceChildren(...(s.cards || []).map((c) => el("article", { class: "card", "data-state": c.state, "aria-label": `${c.label}: ${c.value} ${c.unit || ""}, ${c.state}` },
      el("span", { class: "state", "data-state": c.state }, icon(stateIcon(c.state), "icon sm"), (words[c.state] || c.state).replace(/^\S+ /, "")),
      el("div", { class: "label" }, icon(cardIcon(c.label)), c.label),
      el("div", { class: "big" }, c.value, c.unit ? el("span", { class: "unit", text: c.unit }) : null), el("div", { class: "sub", text: c.sub, title: c.sub }), spark(c.hist))));
    svc.replaceChildren(...(s.services || []).map((x) => el("tr", {},
      el("td", { text: x.Name }), el("td", {}, x.Off && !x.Container ? pill("off") : pill(x.State)),
      el("td", {}, x.Health ? pill(x.Health) : "–"), el("td", { text: tagOf(x.Image) }),
      el("td", { class: "num", text: x.CPU || "" }), el("td", { class: "num", text: x.Mem || "" }),
      el("td", { class: "muted", text: x.Off && !x.Container ? x.Off : x.Status || "" }),
      el("td", { class: "act" }, !x.Off ? el("button", { type: "button", icon: "restart", text: "Restart", onclick: () => act(slot, name, ["restart", x.Name, "--no-pull"], `Recreate ${x.Name} from its current image?`) }) : null))));
    ver.replaceChildren(...(s.versions || []).map((v) => {
      const input = el("input", { placeholder: "version", size: "8", "aria-label": `new version of ${v.Component}` });
      return el("tr", {}, el("td", { text: v.Component }), el("td", { text: v.Pinned || "–" }), el("td", { text: v.Running || "–" }), el("td", { text: v.Previous || "–" }),
        el("td", { class: "muted", text: v.Off ? v.Off : !v.Running ? "not running" : v.Running !== v.Pinned ? "restart to apply" : "✓" }),
        el("td", { class: "act" }, v.Off ? null : [input, " ",
          el("button", { type: "button", icon: "pin", text: "Pin", onclick: () => input.value && act(slot, name, ["pin", v.Component, input.value.trim()], `Pin ${v.Component} to ${input.value}, then pull and recreate it?`) }), " ",
          v.Previous ? el("button", { type: "button", icon: "rollback", text: `Back to ${v.Previous}`, onclick: () => act(slot, name, ["rollback", v.Component], `Roll ${v.Component} back to ${v.Previous}?`) }) : null]));
    }));
    const y = s.system || {};
    sys.replaceChildren(...[["Host", s.host], ["CPU", `${(y.cpu || 0).toFixed(1)} % of ${y.cpus} cores · load ${(y.load1 || 0).toFixed(2)}`], ["Memory", y.mem], ["Swap", y.swap], ["Disk", y.disk], ["Uptime", y.uptime]]
      .flatMap(([k, v]) => [el("dt", { text: k }), el("dd", { text: y.supported || k === "Host" || k === "Disk" ? v || "–" : "needs Linux /proc" })]));
    const z = s.shield || {};
    shieldBox.hidden = !z.declared;
    if (z.declared) sh.replaceChildren(el("p", {}, el("strong", { text: `${z.switch}=${z.on ? "on" : "off"}` }), z.on ? ` — ${z.proxy} asks ${z.addr} before every request.` : ` — ${z.proxy} serves without asking the shield.`),
      el("p", { class: "muted", text: "The shield is a separate program; runsnip only wires the proxy to it. Change it in .env, then reload the proxies. Doctor checks it answers." }));
  };
}

function logsTab(name, body) {
  const pre = el("pre", { class: "log tall", "aria-live": "off" });
  const sel = el("select", { "aria-label": "service" }, el("option", { value: "", text: "all services" }));
  const filter = el("input", { type: "search", placeholder: "filter", "aria-label": "filter" });
  const btn = el("button", { type: "button", class: "primary", text: "Follow" });
  let es = null;
  const stop = () => { if (es) { es.close(); es = null; } btn.textContent = "Follow"; };
  btn.onclick = () => {
    if (es) return stop();
    pre.textContent = "";
    es = new EventSource(`/api/stacks/${encodeURIComponent(name)}/logs?service=${encodeURIComponent(sel.value)}`);
    (live = live || []).push(es);
    btn.textContent = "Stop";
    es.onmessage = (m) => {
      const t = JSON.parse(m.data);
      if (filter.value && !t.toLowerCase().includes(filter.value.toLowerCase())) return;
      pre.append(t + "\n");
      while (pre.childNodes.length > 3000) pre.firstChild.remove();
      pre.scrollTop = pre.scrollHeight;
    };
  };
  sel.onchange = () => { if (es) { stop(); btn.onclick(); } };
  // Service names come from the live snapshot: take one, then close it.
  const snap = new EventSource(`/api/stacks/${encodeURIComponent(name)}/events`);
  snap.onmessage = (m) => { snap.close(); for (const s of JSON.parse(m.data).services || []) if (!s.Off) sel.append(el("option", { value: s.Name, text: s.Name })); };
  (live = live || []).push(snap);
  body.replaceChildren(el("section", { class: "panel" }, el("div", { class: "row" }, sel, btn, filter), pre));
}

// ───────────────────────── .env ─────────────────────────

const filters = [["", "All"], ["missing", "Missing"], ["new", "New"], ["problems", "Problems"], ["secrets", "Secrets"], ["extra", "Extra"]];

async function envTab(name, body, slot) {
  const q = new URLSearchParams(location.hash.split("?")[1] || "");
  let filter = q.get("filter") ?? (store.get("wizard:" + name) ? "missing" : ""), search = "";
  const changes = new Map(); // key → {value?, on?}
  const list = el("div", { class: "envlist" });
  const count = el("span", { class: "muted" });
  const restart = el("input", { type: "checkbox", id: "envrestart" });
  const bar = el("div", { class: "savebar", hidden: true },
    el("span", { id: "envcount" }), el("label", { for: "envrestart" }, restart, " restart the services that read them"),
    el("button", { type: "button", text: "Discard", onclick: () => { changes.clear(); load(); } }),
    el("button", { type: "button", class: "primary", text: "Save", onclick: save }));
  const chips = el("div", { class: "row chips" }, filters.map(([k, l]) => el("button", { type: "button", "aria-pressed": String(k === filter), text: l, onclick: () => { filter = k; for (const b of chips.children) b.setAttribute("aria-pressed", String(b.textContent === l)); load(); } })));
  const box = el("input", { type: "search", placeholder: "search names, values, help", "aria-label": "search" });
  let t = 0;
  box.addEventListener("input", () => { clearTimeout(t); t = setTimeout(() => { search = box.value; load(); }, 200); });
  body.replaceChildren(el("section", { class: "panel" },
    el("div", { class: "row spread" }, el("div", { class: "row search" }, icon("search"), box, count),
      el("div", { class: "row" },
        el("button", { type: "button", text: "Generate missing", title: "runsnip env generate: fills generated values .env lacks", onclick: () => act(slot, name, ["env", "generate"]).then(load) }),
        el("button", { type: "button", text: "Merge the example", title: "runsnip env combine --apply: add the example's new variables", onclick: () => act(slot, name, ["env", "combine", "--apply"], "Write the example's new variables into .env (the old one is kept as .env.bak-<time>)?").then(load) }))),
    chips, list), bar);
  function mark() {
    bar.hidden = changes.size === 0;
    $("#envcount").textContent = `${changes.size} change${changes.size === 1 ? "" : "s"} (values stay out of logs and the audit log)`;
  }
  async function load() {
    list.replaceChildren(el("p", { class: "muted", text: "Loading…" }));
    let r;
    try { r = await api("GET", `/api/stacks/${encodeURIComponent(name)}/env?filter=${encodeURIComponent(filter)}&search=${encodeURIComponent(search)}`); } catch (e) { return list.replaceChildren(errorBox(e)); }
    count.textContent = `${r.vars.length} of ${r.total}`;
    let section = null;
    const rows = [];
    for (const v of r.vars) {
      if (v.section && v.section !== section) { section = v.section; rows.push(el("h3", { class: "section", text: section })); }
      rows.push(envRow(v));
    }
    list.replaceChildren(...(rows.length ? rows : [el("p", { class: "muted", text: filter === "missing" ? "Nothing is missing." : "No variable matches." })]));
    mark();
  }
  function envRow(v) {
    const c = changes.get(v.key) || {};
    const input = el("input", { class: "mono", value: c.value ?? (v.secret ? "" : v.value), placeholder: v.secret && v.value ? v.value : v.example || "", spellcheck: "false", autocomplete: "off", "aria-label": v.key });
    if (v.written) input.readOnly = true;
    const on = el("input", { type: "checkbox", "aria-label": `${v.key} enabled` });
    on.checked = c.on ?? (v.enabled || v.new);
    const problem = el("div", { class: "bad small", text: v.problem || "" });
    const help = el("pre", { class: "help", hidden: true, text: v.help || "No help in the example." });
    const badges = [v.new && ["new", "new, needs a value"], v.extra && ["extra", "no longer in the example"], v.secret && ["secret", "secret"], v.optional && ["opt", "optional"], v.written && ["written", "written by runsnip"], v.problem && ["problem", "problem"], !v.enabled && v.inEnv && ["off", "commented out"]].filter(Boolean);
    const set = (patch) => { changes.set(v.key, { ...(changes.get(v.key) || {}), ...patch }); mark(); };
    input.addEventListener("input", () => set({ value: input.value }));
    input.addEventListener("change", async () => {
      try { const r = await api("POST", `/api/stacks/${encodeURIComponent(name)}/env/validate`, { key: v.key, value: input.value }); problem.textContent = r.problem; } catch { /* checked again on save */ }
    });
    on.addEventListener("change", () => set({ on: on.checked }));
    return el("div", { class: "envrow", "data-problem": String(!!v.problem) },
      el("div", { class: "envhead" }, on, el("strong", { class: "mono", text: v.key }), badges.map(([k, l]) => el("span", { class: `badge ${k}`, text: l })),
        v.services && v.services.length ? el("span", { class: "muted small", text: `read by ${v.services.join(", ")}` }) : null),
      el("div", { class: "row" }, input,
        v.secret && v.value ? el("button", { type: "button", icon: "reveal", text: "Reveal", onclick: async () => {
          try { const r = await api("POST", `/api/stacks/${encodeURIComponent(name)}/env/reveal`, { key: v.key }); input.value = r.value; input.placeholder = ""; } catch (e) { problem.textContent = e.message; }
        } }) : null,
        v.generate ? el("button", { type: "button", icon: "env", text: "Generate", title: v.generate, onclick: () => act(slot, name, ["env", "generate", v.key, ...(v.value ? ["--force"] : [])], v.value ? `Replace ${v.key} with a new generated value?` : null).then(load) }) : null,
        el("button", { type: "button", class: "link", text: "Help", "aria-expanded": "false", onclick: (e) => { help.hidden = !help.hidden; e.target.setAttribute("aria-expanded", String(!help.hidden)); } })),
      problem, help);
  }
  async function save() {
    const vals = {}, cmd = ["env", "set", "--stdin"];
    const keys = [];
    for (const [k, c] of changes) {
      if (c.value !== undefined) vals[k] = c.value;
      if (c.on === true) cmd.push("--on", k);
      if (c.on === false) cmd.push("--off", k);
      keys.push(k);
    }
    if (restart.checked) cmd.push("--restart");
    const j = await act(slot, name, cmd, null, JSON.stringify(vals));
    if (!j) return;
    if (j.status === "ok") {
      changes.clear();
      await load();
      if (!restart.checked) offerRestart(name, slot, keys);
    }
  }
  load();
}

// offerRestart: after a save without --restart, the running services reading the changed keys — one click restarts
// just those. Services that are not running take the new values when they start.
function snapshot(name) {
  return new Promise((resolve) => {
    const es = new EventSource(`/api/stacks/${encodeURIComponent(name)}/events`);
    const t = setTimeout(() => { es.close(); resolve(null); }, 15000);
    es.onmessage = (m) => { clearTimeout(t); es.close(); resolve(JSON.parse(m.data)); };
  });
}
async function offerRestart(name, slot, keys) {
  let r, snap;
  try { [r, snap] = await Promise.all([api("GET", `/api/stacks/${encodeURIComponent(name)}/env`), snapshot(name)]); } catch { return; }
  const running = new Set(((snap && snap.services) || []).filter((s) => s.State === "running").map((s) => s.Name));
  const readers = [...new Set(r.vars.filter((v) => keys.includes(v.key)).flatMap((v) => v.services || []))];
  const svcs = readers.filter((s) => running.has(s));
  if (!readers.length) return;
  const out = el("div");
  if (!svcs.length) {
    slot.append(el("div", { class: "panel" }, el("span", { class: "muted", text: `Saved. Nothing running reads these variables: ${readers.join(", ")} take them when they start.` })));
    return;
  }
  slot.append(el("div", { class: "panel row" }, el("span", { text: `Saved. Running and reading these variables: ${svcs.join(", ")}.` }),
    el("button", { type: "button", class: "primary", text: `Restart ${svcs.length === 1 ? svcs[0] : "those " + svcs.length}`, onclick: async (e) => {
      e.target.disabled = true;
      for (const s of svcs) { const j = await act(out, name, ["restart", s, "--no-pull"]); if (!j || j.status !== "ok") break; }
    } }), out));
}

// ───────────────────────── jobs, admins, updates ─────────────────────────

async function jobsList(body, stack) {
  let list;
  try { list = await api("GET", "/api/jobs" + (stack ? `?stack=${encodeURIComponent(stack)}` : "")); } catch (e) { return body.replaceChildren(errorBox(e)); }
  body.replaceChildren(el("section", { class: "panel" }, el("h2", { text: "Jobs" }),
    el("p", { class: "muted", text: "Every change, from this page, ssh or the console: who ran what, when, and how it ended." }),
    el("div", { class: "scroll" }, el("table", {}, el("thead", {}, el("tr", {}, ["When", "Stack", "Command", "Who", "Via", "Outcome"].map((h) => el("th", { text: h })))),
      el("tbody", {}, list.map((j) => el("tr", {},
        el("td", {}, el("a", { href: `#/job/${encodeURIComponent(j.id)}`, text: ago(j.started) })), el("td", { text: !j.stack || j.stack.startsWith("_") ? "–" : j.stack }),
        el("td", { class: "mono", text: j.command.join(" ") }), el("td", { text: j.user }), el("td", { text: j.via }),
        el("td", {}, pill(j.status, j.status + (j.exit ? ` (${j.exit})` : ""))))))))));
}

async function jobPage(id) {
  try {
    const j = await api("GET", `/api/jobs/${encodeURIComponent(id)}`);
    const v = jobView(j);
    page("Job", el("p", { class: "muted", text: `${j.user} via ${j.via}, ${when(j.started)}` }), v.panel);
  } catch (e) { page("Job", errorBox(e)); }
}

async function adminsPage() {
  const slot = el("div"), audit = el("div");
  let list;
  try { list = await api("GET", "/api/admins"); } catch (e) { return page("Admins", errorBox(e)); }
  const nm = el("input", { placeholder: "name", "aria-label": "new admin's name", pattern: "[a-z0-9][a-z0-9._-]{0,31}" });
  page("Admins", el("section", { class: "panel" }, el("h2", { text: "Admins" }),
    el("div", { class: "scroll" }, el("table", {}, el("thead", {}, el("tr", {}, ["Name", "Passkeys", "TOTP", "Since", ""].map((h) => el("th", { text: h })))),
      el("tbody", {}, list.map((a) => el("tr", {}, el("td", { text: a.name + (a.name === me.name ? " (you)" : "") }), el("td", { text: String(a.passkeys) }), el("td", { text: a.totp ? "yes" : "–" }),
        el("td", { text: a.pending ? "waiting for enrolment" : when(a.created) }),
        el("td", { class: "act" },
          el("button", { type: "button", text: "Reset", onclick: () => act(slot, "", ["admin", "reset", a.name], `Remove ${a.name}'s passkeys and TOTP and make a new enrolment code?`) }), " ",
          el("button", { type: "button", text: "Remove", onclick: () => act(slot, "", ["admin", "remove", a.name], `Remove ${a.name}?`) }))))))),
    el("form", { class: "row", onsubmit: (e) => { e.preventDefault(); if (nm.value) act(slot, "", ["admin", "add", nm.value.trim()]); } },
      nm, el("button", { text: "Add an admin" }), el("span", { class: "muted small", text: "The job prints a one-time enrolment code to hand over." })), slot),
  el("section", { class: "panel" }, el("h2", { text: "Audit log" }), el("button", { type: "button", text: "Show the latest", onclick: async () => {
    try {
      const recs = await api("GET", "/api/audit");
      audit.replaceChildren(el("div", { class: "scroll" }, el("table", {}, el("tbody", {}, recs.reverse().map((r) => el("tr", {},
        el("td", { text: ago(r.time || r.at) }), el("td", { text: r.event }), el("td", { text: r.user || "" }), el("td", { text: r.via || "" }), el("td", { class: "mono small", text: Object.entries(r).filter(([k]) => !["time", "at", "event", "user", "via"].includes(k)).map(([k, v]) => `${k}=${typeof v === "object" ? JSON.stringify(v) : v}`).join(" ") })))))));
    } catch (e) { audit.replaceChildren(errorBox(e)); }
  } }), audit));
}

async function updatesPage() {
  const slot = el("div");
  const ver = el("input", { placeholder: "latest", "aria-label": "version to install", size: "10" });
  const last = store.get("update:job");
  page("Updates", el("section", { class: "panel" }, el("h2", { text: "runsnip" }),
    el("p", {}, "This server runs ", el("strong", { class: "mono", text: me.version }), "."),
    el("div", { class: "row" },
      el("button", { type: "button", icon: "updates", text: "Check for updates", onclick: () => act(slot, "", ["self", "check"]) }),
      ver, el("button", { type: "button", class: "primary", icon: "updates", text: "Update", onclick: () => selfUpdate(slot, ver.value.trim()) }),
      el("button", { type: "button", icon: "rollback", text: "Roll back", onclick: () => selfUpdate(slot, "", true) })),
    el("p", { class: "muted small", text: "Updating downloads the release, checks its checksum, swaps the binary atomically (the old one kept as runsnip.previous) and restarts this page's server. If the new one does not answer in time, the old one comes back by itself. Updates wait while a stack is being changed." }),
    slot));
  if (last) {
    store.set("update:job", null);
    try { const j = await api("GET", `/api/jobs/${encodeURIComponent(last)}`); slot.replaceChildren(jobView(j).panel); } catch { /* gone */ }
  }
}

// selfUpdate runs the update as a job; when the server goes away to restart, an overlay waits for /healthz and reloads.
async function selfUpdate(slot, version, rollback) {
  const cmd = rollback ? ["self", "rollback"] : ["self", "update", ...(version ? ["--version", version] : [])];
  if (!(await ask(rollback ? "Go back to the previous runsnip and restart?" : `Update runsnip to ${version || "the latest release"} and restart?`, rollback ? "Roll back" : "Update"))) return;
  let j;
  try { j = await runJob("", cmd); } catch (e) { return slot.replaceChildren(errorBox(e)); }
  store.set("update:job", j.id);
  const old = me.version;
  let waiting = false;
  const v = jobView(j, { onDrop: () => { if (!waiting) { waiting = true; restarting(old, version, j.id); } } });
  slot.replaceChildren(v.panel);
  v.ended.then((end) => { if (!waiting && end.status !== "ok") store.set("update:job", null); if (!waiting && end.status === "ok") restarting(old, version, j.id); });
}

function restarting(old, wanted, jobId) {
  const o = $("#overlay"), text = $("#overlaytext"), actb = $("#overlayact");
  o.hidden = false; actb.replaceChildren();
  $("#overlaytitle").textContent = "runsnip is restarting…";
  text.textContent = `From ${old}${wanted ? " to " + wanted : ""}. This page reloads by itself.`;
  const started = Date.now();
  const tick = async () => {
    try {
      const r = await fetch("/healthz", { cache: "no-store" });
      if (r.ok) {
        const h = await r.json();
        if (h.version !== old) { location.reload(); return; }
        // Same version answering: the update may have been rolled back — the job says.
        try {
          const j = await api("GET", `/api/jobs/${encodeURIComponent(jobId)}`);
          if (j.status !== "running") { o.hidden = true; location.hash = "#/updates"; updatesPage(); return; }
        } catch { /* signed out by the restart: reload */ location.reload(); return; }
      }
    } catch { /* not up yet */ }
    if (Date.now() - started > 90000) {
      text.textContent = `It is taking longer than expected. Was ${old}${wanted ? ", asked for " + wanted : ""}.`;
      actb.replaceChildren(el("button", { type: "button", class: "primary", text: "Reload", onclick: () => location.reload() }));
      return;
    }
    setTimeout(tick, 1000);
  };
  setTimeout(tick, 1500);
}

// ───────────────────────── routing ─────────────────────────

async function route() {
  stopLive();
  const h = decodeURIComponent(location.hash.replace(/^#\/?/, "").split("?")[0]);
  const parts = h.split("/").filter(Boolean);
  if (!parts.length) return stacksPage();
  switch (parts[0]) {
    case "add": return addPage();
    case "s": return stackPage(parts[1], parts[2]);
    case "jobs": { const b = el("div"); page("Jobs", b); return jobsList(b); }
    case "job": return jobPage(parts[1]);
    case "admins": return adminsPage();
    case "updates": return updatesPage();
  }
  page("Not found", el("p", { text: "Nothing here." }), el("a", { href: "#/", text: "Stacks" }));
}

async function boot() {
  stopLive();
  let s;
  try { s = await api("GET", "/api/status"); } catch (e) { return page("", errorBox(e)); }
  me = { csrf: s.csrf || "", name: s.name || "", version: s.version };
  $("#host").textContent = location.hostname;
  $("#nav").hidden = !(s.signedIn && !s.enrolling);
  if (s.needsSetup && !s.signedIn) return setupPage();
  if (!s.signedIn) return loginPage();
  if (s.enrolling) return enrolPage();
  route();
}

loadIcons();
window.addEventListener("hashchange", () => { if (me.csrf) route(); });
$("#signout").addEventListener("click", async () => { try { await api("POST", "/api/logout"); } catch { /* gone anyway */ } me = { csrf: "" }; boot(); });
boot();
