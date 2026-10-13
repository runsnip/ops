// screens.mjs — the page at desktop and phone width, checked and photographed (a development machine, not CI-only).
//
//   PLAYWRIGHT=<folder that resolves "playwright"> node scripts/screens.mjs [--stack testdata/stacks/tiny] [--out dir]
//
// Builds runsnip, starts `runsnip serve` on a fresh state folder with the stack registered, creates the first admin
// through the API (setup code → TOTP), then opens the stack's Graph tab: at 1280×800 the canvas shows one box per
// node of `runsnip graph --json`; at 390×844 the canvas is hidden and the same nodes are a list grouped by layer.
// Then the Backups tab at both widths: Download saves the backup and the audit log records it.
import { execFileSync, spawn } from "node:child_process";
import { createHmac } from "node:crypto";
import { createRequire } from "node:module";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, "..");
const arg = (n, d) => { const i = process.argv.indexOf(`--${n}`); return i >= 0 ? process.argv[i + 1] : d; };
const stackDir = path.resolve(root, arg("stack", "testdata/stacks/tiny"));
const out = path.resolve(arg("out", path.join(os.tmpdir(), "runsnip-screens")));
const { chromium } = createRequire(path.join(process.env.PLAYWRIGHT ?? root, "package.json"))("playwright");

fs.mkdirSync(out, { recursive: true });
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "runsnip-screens-"));
const bin = path.join(tmp, "runsnip");
execFileSync("go", ["build", "-o", bin, "."], { cwd: root, stdio: "inherit" });
const state = path.join(tmp, "state");
fs.mkdirSync(state, { mode: 0o700 });
const name = path.basename(stackDir);
// A copy of the stack with one backup in its folder, for the Backups tab's Download.
const stackCopy = path.join(tmp, name);
fs.cpSync(stackDir, stackCopy, { recursive: true });
const backups = path.join(tmp, "backups");
fs.mkdirSync(backups, { mode: 0o700 });
fs.appendFileSync(path.join(stackCopy, "stack.yaml"), `\nbackup:\n  dir: ${backups}\n`);
const backupName = "db-20261013-000000.sql.gz", backupBody = "a backup's bytes\n";
fs.writeFileSync(path.join(backups, backupName), backupBody, { mode: 0o600 });
fs.writeFileSync(path.join(state, "stacks.json"), JSON.stringify([{ name, dir: stackCopy, data: path.join(tmp, "data"), added: new Date().toISOString() }]));
const env = { ...process.env, RUNSNIP_STATE: state };
const graph = JSON.parse(execFileSync(bin, ["--stack", name, "graph", "--json"], { env }).toString());
const code = /Setup code: (\S+)/.exec(execFileSync(bin, ["admin", "setup-code"], { env }).toString())[1];
const port = 7400 + Math.floor(Math.random() * 500);
const serve = spawn(bin, ["serve", "--listen", `127.0.0.1:${port}`], { env, stdio: "ignore" });
const base = `http://localhost:${port}`;
let failed = 0;
const check = (ok, what) => { console.log(`${ok ? "✓" : "✗"} ${what}`); if (!ok) failed++; };
try {
  for (let i = 0; i < 50; i++) { try { if ((await fetch(`${base}/healthz`)).ok) break; } catch { /* starting */ } await new Promise((r) => setTimeout(r, 100)); }
  let cookie = "", csrf = "";
  const call = async (method, p, body) => {
    const r = await fetch(base + p, { method, headers: { "Content-Type": "application/json", Origin: base, "X-CSRF-Token": csrf, Cookie: cookie }, body: body && JSON.stringify(body) });
    const c = r.headers.get("set-cookie"); if (c) cookie = c.split(";")[0];
    return r.json();
  };
  csrf = (await call("POST", "/api/setup", { code, name: "ops" })).csrf;
  const { secret } = await call("POST", "/api/totp/begin");
  const key = b32(secret), step = Buffer.alloc(8); step.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000)));
  const h = createHmac("sha1", key).update(step).digest(), o = h[h.length - 1] & 15;
  await call("POST", "/api/totp/confirm", { code: String((h.readUInt32BE(o) & 0x7fffffff) % 1e6).padStart(6, "0") });

  const browser = await chromium.launch();
  for (const [label, w, hgt] of [["desktop", 1280, 800], ["phone", 390, 844]]) {
    const ctx = await browser.newContext({ viewport: { width: w, height: hgt }, deviceScaleFactor: 2 });
    await ctx.addCookies([{ name: cookie.split("=")[0], value: cookie.split("=")[1], url: base }]);
    const page = await ctx.newPage();
    const errors = [];
    page.on("pageerror", (e) => errors.push(e.message));
    page.on("console", (m) => { if (m.type() === "error" && !/EventSource|events/.test(m.text())) errors.push(m.text()); });
    await page.goto(`${base}/#/s/${name}/graph`);
    if (label === "desktop") {
      await page.waitForSelector(".gcanvas .node");
      check(await page.locator(".gcanvas .node").count() === graph.nodes.length, `${label}: ${graph.nodes.length} nodes drawn`);
      check(await page.locator(".gcanvas .edge").count() > 0, `${label}: edges drawn`);
      check(await page.locator(".gcanvas .node use").count() === graph.nodes.length, `${label}: every node has its icon`);
      await page.locator(".gcanvas .node").first().click();
      check(await page.locator(".gside:not([hidden])").count() === 1, `${label}: a click opens the side panel`);
    } else {
      await page.waitForSelector(".glist .gitem");
      check(await page.locator(".gcanvas").isHidden(), `${label}: the canvas gives way to a list`);
      check(await page.locator(".glist .gitem").count() === graph.nodes.length, `${label}: ${graph.nodes.length} nodes listed by layer`);
      const sw = await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1);
      check(sw, `${label}: no horizontal scroll`);
    }
    await page.screenshot({ path: path.join(out, `graph-${name}-${label}.png`), fullPage: label === "phone" });
    // Backups: the list, then Download (the session is fresh: no re-authentication asked) saves the file.
    await page.goto(`${base}/#/s/${name}/backups`);
    await page.getByText(backupName).waitFor();
    if (label === "phone") check(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `${label}: Backups has no horizontal scroll`);
    const [dl] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: "Download" }).click()]);
    check(dl.suggestedFilename() === backupName && fs.readFileSync(await dl.path(), "utf8") === backupBody, `${label}: Download saves ${backupName} as it is on the server`);
    await page.screenshot({ path: path.join(out, `backups-${name}-${label}.png`), fullPage: label === "phone" });
    check(errors.length === 0, `${label}: no script errors${errors.length ? ": " + errors.join("; ") : ""}`);
    await ctx.close();
  }
  await browser.close();
  const audit = fs.readFileSync(path.join(state, "audit.log"), "utf8");
  check((audit.match(/"event":"backup.download"/g) || []).length === 2 && audit.includes('"complete":true'), "every download is in the audit log");
  const web = path.join(root, "internal/serve/web");
  const size = (f) => fs.statSync(path.join(web, f)).size;
  console.log(`bytes: graph.js ${size("graph.js")}, icons.svg ${size("icons.svg")}, app.js ${size("app.js")}, app.css ${size("app.css")} → ${out}`);
} finally {
  serve.kill();
  fs.rmSync(tmp, { recursive: true, force: true });
}
process.exit(failed ? 1 : 0);

function b32(s) {
  const a = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
  let bits = "";
  for (const c of s.replace(/=+$/, "")) bits += a.indexOf(c).toString(2).padStart(5, "0");
  const bytes = [];
  for (let i = 0; i + 8 <= bits.length; i += 8) bytes.push(parseInt(bits.slice(i, i + 8), 2));
  return Buffer.from(bytes);
}
