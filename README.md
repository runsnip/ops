# runsnip — stack operations

`runsnip` runs a Docker Compose stack on its own servers: a terminal dashboard with tabs, the scriptable commands
(start, restart, pin, rollback, doctor, backup, env…), the Caddy wiring for an optional request shield, a web front end for the whole server, and on the build machine `runsnip build`. One Go binary, no Node, no project-specific
script: what a project does differently is written in its `stack.yaml`.

It is stack-agnostic. **[STANDARD.md](STANDARD.md)** is the runsnip stack standard (v1) it implements;
`schema/stack.v1.json` is the stack file's schema. `runsnip adopt` drafts a `stack.yaml` for an existing project.

## On a server

The server never builds it. A project's `scripts/publish-infra.sh` builds `bin/runsnip-linux-amd64` and
`bin/runsnip-linux-arm64` into its infra branch, next to the standard `runsnip` shim (`shim/runsnip`), which runs
the one for the machine — the server needs `sh` and `uname`, nothing else.

```bash
git clone --branch infra --single-branch --depth 1 <repo> stack && cd stack
cp .env.example .env && runsnip env generate     # every generated secret and key pair .env lacks
sudo ./runsnip install                           # /usr/local/bin/runsnip
runsnip init && runsnip login && runsnip start
runsnip doctor
```

| Command | |
|---|---|
| `runsnip` / `runsnip top [--once] [--width N]` | dashboard: Overview (CPU, RAM, network, services healthy as four big numbers with their last minutes), Services, Versions, Logs, Doctor, Backups, Infra, Env. `--once` prints every tab and exits |
| `runsnip infra pull` | fast-forward the infra checkout and say what each changed file needs (start, reload, env combine…) |
| `runsnip start [component]` | git pull → pull → up (or the stack's order) → recreate the proxies → prune; with a component: that one alone |
| `runsnip restart <svc> [--no-pull]` | pull and recreate one service, never its dependencies; services sharing its image (migrations) first |
| `runsnip reload` | recreate the proxies so they read their config files again |
| `runsnip stop [svc]` | the stack down, or one service stopped |
| `runsnip pin [component\|all] <version>` / `rollback [component]` | the pin in `.env`, the one before kept in `<TAG_VAR>_PREVIOUS` |
| `runsnip version` | pinned, running and previous version of every component, and why one is off |
| `runsnip ps` · `logs [svc] [-n N] [--no-follow]` · `health [svc]` | |
| `runsnip doctor [--json]` | every check; a failed probe is reported and the doctor goes on; exit 1 on failures |
| `runsnip env generate` · `env combine` | fill generated values (never overwriting) · merge a new example with `.env` for review |
| `runsnip psql [statement]` · `exec [--root] [svc] [cmd…]` · `backup` · `prune` | |
| `runsnip <download> [get <item…>]` | a stack's own downloads (`downloads:` in stack.yaml, e.g. `models` — model files on the data disk) |
| `runsnip shield snippet` | the request shield's Caddy snippet, as the standard's `(shield_on)` / `(shield_off)` |
| `runsnip login` · `init` · `install` | |
| `runsnip validate [dir]` · `adopt [dir] [--print]` | the stack against the schema and the standard · a draft `stack.yaml` for a project |
| `runsnip serve [--listen addr] [--public] [--origin url] [--allow cidrs]` | the web front end (below) |
| `runsnip build [--dry-run] [--status] [component…]` | on the build machine: pick components, release them, follow the CI runs |

The stack folder is `RUNSNIP_HOME`, else the folder holding the program (`bin/..`), else the current one;
`RUNSNIP_COMPOSE_FILE` and `RUNSNIP_ENV_FILE` point at other files.

## A server with several stacks, and the web page

`install.sh` (attached to every release) installs `/usr/local/bin/runsnip` after checking it against the release's
`SHA256SUMS`, creates the state folder `/var/lib/runsnip` (700) and `runsnip-serve.service` on 127.0.0.1:7070, and
prints a one-time setup code (15 minutes) while the server has no admin:

```bash
curl -fsSL https://github.com/runsnip/ops/releases/latest/download/install.sh | sudo sh
```

| Command | |
|---|---|
| `runsnip stack list` · `check-paths --name N --dir D --data P [--json]` | the stacks of this server · whether paths may hold one, each refusal with its reason |
| `runsnip stack keygen <name>` · `add <name> --repo URL [--branch B] --dir D --data P` · `remove <name> [--down]` | a read-only deploy key · clone, read stack.yaml, register |
| `runsnip --stack <name> <command…>` | any stack command against one of them |
| `runsnip jobs [-n N]` · `jobs show <id>` | every change from the page, ssh or the console: who, what, when, the log, the outcome; one at a time per stack (`--wait 5m` waits) |
| `runsnip admin setup-code [--ttl] \| add \| reset \| remove \| list` | the page's admins (passkey, or TOTP) |
| `runsnip self check [--json]` · `self update [--version vX] [--wait]` · `self rollback` · `self health` | update from GitHub Releases: checksum, atomic swap, `runsnip.previous` kept, serve restarted and rolled back when it does not answer |
| `runsnip env list [--filter new\|missing\|secrets\|extra\|problems] [--search s] [--json]` · `env set KEY=V… \| --stdin [--restart]` · `env generate [KEY…] [--force]` · `env combine [--diff\|--apply]` | `.env` as a model: help from the example's comments, validation from stack.yaml, which services read each key |
| `runsnip graph [--json]` | the stack as nodes and edges (below) |

`runsnip serve` is the web front end of the server. It never does the work: every action is one of the commands
above, started as a job by the same binary (values such as `.env` entries travel on stdin, never on the command
line), and its log streams to the page from disk — it survives a restart of serve. Nobody gets in without an admin's
passkey (WebAuthn) or TOTP; the first admin comes with the setup code, which only works until an admin exists.
Sessions are short (30 min idle, 12 h at most), dangerous actions need an authentication of the last 5 minutes
(`--idle` and `--reauth-after` may only shorten them), sign-in guesses are rate-limited, every write checks a CSRF
token and the Origin, the CSP is `default-src 'none'` with the page's own files only, `--allow` takes an IP
allowlist, and `/var/lib/runsnip/audit.log` is append-only. Sessions persist by the SHA-256 of their cookie, so an
update does not sign anyone out. It listens on 127.0.0.1 unless `--public`; put Caddy in front on a hostname.

Nothing runs while nobody watches: a stack's sampler starts with the first open page and stops with the last.
Containers and their stats come from the Docker Engine API on its socket (no `docker` processes). Measured
(`/proc/<pid>/stat` utime+stime of serve and its children, VmRSS):

| | before (docker CLI polling) | after (Engine API) |
|---|---|---|
| idle, no page | 16.8 MB, 0 ms / 30 s | 16.3 MB, 0 ms / 30 s |
| one page on a stack's overview | 17.9 MB, 3.79 s / 30 s (runsnip 0.13 s + docker CLI 3.66 s) | 18.3 MB, 0.54 s / 30 s, no child process |

Before: a 23-service stack on a local replica, linux/arm64 in a container, host load 3.8–4.5. After: a fresh Ubuntu 24.04 VM
(systemd and Docker in a privileged container on Docker Desktop, 8 vCPUs), 23 services, host load 16–22 from other
work — the CPU figure is an upper bound.

### The graph

`runsnip graph --json` reads only what the stack declares and draws nothing it does not find there:

- **nodes**: every compose service (proxies by image or `proxies:`, datastores by image or `graph.datastores`), the
  hostnames the proxies serve (Caddyfile site addresses), the outside hosts the services' environment points at
  (grouped by registrable domain) and the registries the images come from;
- **edges**: `route` (hostname → proxy → proxy → service, from the Caddyfile — `reverse_proxy`, `handle`, snippets,
  matchers — and nginx — `server_name`, `location`, `upstream`, `set`, `map` — with the path as the label), `depends`
  (compose), `calls` and `data` (an environment value naming another service by any of its names, as a URL,
  `host:port`, or a bare name under a `*_HOST`-like key);
- **layers** left to right: ingress → edge → app → data → external, deeper columns for chains; within a column the
  order follows what points at it, then the name — the same stack always gives the same picture.

`services.<name>.graph: {kind, layer, group, icon, hide}` and `graph: {accent, datastores, ignore}` in stack.yaml
adjust it. The page's Graph tab draws the same JSON (pan, zoom, drag — positions kept per stack in the state folder,
"Reset layout" forgets them —, a side panel with details, logs, restart and pin through jobs, upstream/downstream on
hover, failing nodes and the edges into them in red); on a phone it is a list grouped by layer.
`scripts/screens.mjs` checks and photographs both widths.

### Icons

The page's icons are [@runsnip/icons](https://icons.runsnip.net) 0.4.0 (MIT): `scripts/icons/generate.mjs` takes only
the icons `scripts/icons/icons.json` names from that pinned version and writes them as one sprite,
`internal/serve/web/icons.svg` (55 icons, 17.4 KB, 2.8 KB gzipped — the package's full UMD build is 355 KB). It
installs the pinned version from npm; `RUNSNIP_ICONS_PKG=<folder>` (or `--pkg <folder>`) uses a built local checkout
instead — needed until 0.4.0 is on npm. An entry may be marked `placeholder` while the set lacks its drawing.

## The request shield

A stack may declare one (`shield: {addr_env, addr, check_path, copy_headers, proxy}` in stack.yaml); a stack
without the block has none. Its operator runs the shield; runsnip never installs, configures, runs or inspects it:

- the Caddyfile carries `(shield_on)` (a `forward_auth` to it) and an empty `(shield_off)`, and every site imports
  `shield_{$SHIELD:off}` (`runsnip shield snippet` prints them from the block);
- opt-in: with `SHIELD=on` in `.env` (and `runsnip reload`) Caddy asks it before every request;
- `runsnip doctor` checks the route, not the shield: it asks the address from inside the proxy's container and fails
  with "every site returns 502" when nothing answers there.

## Development

```bash
go test ./...                 # every package; the doctor, stack and TUI against a fake docker (internal/run.Fake)
go test ./schema -update      # after changing internal/def: regenerate schema/stack.v1.json (a test fails while stale)
go run . validate ../         # a stack folder against the standard
```

Layout: `main.go` (commands) · `internal/def` (stack.yaml) · `internal/compose` (reads compose files without docker)
· `internal/stack` (operations) · `internal/doctor` (the framework and its check types) · `internal/keygen`
(`env generate`) · `internal/envform` (`env combine`, the Env tab) · `internal/shield` (the Caddy snippet) · `internal/release`
(`runsnip build`) · `internal/standard` (rules, `validate`, `adopt`) · `internal/tui` · `internal/serve` (the page,
`web/`) · `internal/graph` · `internal/auth` · `internal/jobs` · `internal/paths` · `internal/dockerapi` · `schema/` ·
`shim/` · `scripts/` (release, icons, screens).

`lint_test.go` fails when anything in this repository names a project runsnip serves: the names come from a deny
list kept outside it (`RUNSNIP_LINT_DENYLIST=<file>`, one literal per line; skipped when unset). `fixtures_test.go`
runs the whole tool on a synthetic stack in testdata and on any stack given as
`RUNSNIP_FIXTURES=name=<stack folder>@<expectations.json>[:…]` — other projects' stacks and expectations stay in
those projects.

The repository is private, like every repository under runsnip/.
