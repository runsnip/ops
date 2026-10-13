# runsnip stack standard — v1

How a project runs a Docker Compose stack on its own servers so that one tool, `runsnip`, can install, start, pin,
roll back, check, back up and watch it — with no per-project script. `runsnip validate` checks a stack against it;
`runsnip adopt` drafts a `stack.yaml` for an existing project.

- **Version:** 1 (2026-10-12). A stack says which version it follows: `standard: 1` in its `stack.yaml`. A later
  version adds rules; it never changes what a v1 stack means.
- **Schema:** `schema/stack.v1.json` (JSON Schema 2020-12), generated from the tool's own types, so the two cannot
  drift.
- **Checking it:** `runsnip validate` checks a stack against the schema and every rule below, and prints each gap with
  its fix and each override with the change that would drop it. `runsnip adopt` drafts the `stack.yaml` of a project
  that does not have one yet, with the same report.

Each rule is **required** (a stack that breaks it does not conform) or a **default** (what the tool assumes when the
stack file says nothing; a stack that differs writes one key in `stack.yaml` and still conforms). Every rule has its
id in `code`, the id `validate` prints.

The guiding idea: **detect, then default, then override.** Whatever can be read from the compose file (components,
images, profiles, proxies, the database, health checks, bind mounts) is read from it; what cannot is a default of
this standard; what a project does differently is one key in its stack file.

## 1. Layout

| Rule | | Why |
|---|---|---|
| `layout` — The project's infra folder (by default `infra/`, or `infra/<name>/`) holds `stack.yaml`, the compose file, `.env.example`, the proxies' config files and the `runsnip` shim. It is what the infra branch ships. | required | The server checks out one folder and needs nothing else to run the stack. |
| `stack-file` — `stack.yaml` declares `standard: 1` and passes `schema/stack.v1.json`. Unknown keys are errors. | required | The tool knows which rules apply, and a typo fails on the build machine, not on a server. |

```
infra/
  stack.yaml              the stack definition (often a dozen lines, more for project-specific doctor checks)
  docker-compose.yml      or compose.yaml
  .env.example            every variable, with the comment that explains it
  Caddyfile, nginx/…      the proxies' configuration, mounted read-only
  shield/shield.yaml      the request shield's policy for its operator (optional)
  runsnip                 the standard shim (shim/runsnip of this repository)
  ops/                    runsnip's source — a submodule in the repository, never on the infra branch
```

A host may add `stack.local.yaml` beside `stack.yaml`: its keys replace the stack file's (a replica pulls nothing and
starts services in its own order). It is the stack file's counterpart of `docker-compose.override.yml`.

## 2. Compose

| Rule | | Why |
|---|---|---|
| `compose-file` — The compose file has one of docker compose's own names (`compose.yaml`, `compose.yml`, `docker-compose.yaml`, `docker-compose.yml`); `docker-compose.override.yml` beside it holds a host's differences. Override: `compose:`. | default | docker compose finds it without `-f`, and the override is the only place a host differs. |
| `compose-name` — The compose file sets the project `name:`. | required | Container, network and volume names do not depend on what the checkout's folder is called; the data folder defaults to `/srv/<name>`. |
| `service-names` — Service names are lowercase words joined by hyphens; `container_name`, when set, equals the service name. | default | `docker logs <x>` and `runsnip logs <x>` name the same container. |
| `profiles` — A service behind a compose profile has that profile in `x-runsnip-profiles`, mapping it to the `.env` variable that turns it on (`media: MEDIA_WORKER_TOKEN`). Override: `profiles_block:` or `profiles:`. | required | Every command turns the same profiles on, so `stop` and `ps` see what `start` started. |

## 3. Logging limits

| Rule | | Why |
|---|---|---|
| `logging` — Every service logs with the `json-file` (or `local`) driver capped by `max-size` and `max-file`; the default is one `x-logging` anchor of 10 MB × 3. | required | Docker's logs have no cap otherwise, and the symptom when the disk fills is "disk full", not "too many logs". |

## 4. Images, tags and pins

| Rule | | Why |
|---|---|---|
| `images` — A released component's image is `<repo>:${<IMAGE>_TAG:-<default>}`: one variable per image. Services sharing an image (a `migrate` job beside `api`) are the same component; the one named like the image is its main service, the others are pulled and recreated before it. | required | The pin is one line of `.env`, and the tool finds every component from compose alone. |
| `tag-names` — The variable is the image's name upper-cased with `_TAG` (`image-worker` → `IMAGE_WORKER_TAG`). | default | The variable can be told from the image without reading compose. Any name works. |
| `pins` — Versions are pinned in `.env`. `runsnip pin <component> <version>` keeps the one before in `<TAG_VAR>_PREVIOUS`; `rollback` goes back to it. `.last-tag-<component>` files (the older form) are still read. Override: `components.previous: file`. | default | All of a server's state that is not data is one file, backed up and combined like the rest of `.env`. |

`pin <version>` without a component pins the first component in compose (override: `components.default`).
`pin all <version>` pins every enabled component and starts the stack.

## 5. Health

| Rule | | Why |
|---|---|---|
| `health-check` — Every component declares a compose `healthcheck`. A component that declares it in its image instead says so (`services.<name>.health.image: true`); a worker with nothing to ask says why (`services.<name>.health.none: <why>`). | required | Docker restarts what is unhealthy, `ps` shows it, and the doctor and the dashboard count it. |
| `health-endpoint` — The health endpoint is `GET /health` on `$PORT`, answering `200` (body free, `{"ok":true}` by convention) when the service can serve — its own dependencies included only when it cannot serve without them. Probe it with busybox `wget` (`["CMD", "wget", "-q", "-T", "2", "-O", "/dev/null", "http://127.0.0.1:<port>/health"]`), not by starting an interpreter. Override: `services.<name>.health: {path, port}` or `{cmd: […]}`. | default | One probe for every service; no per-service knowledge in the tools. |

## 6. Environment

| Rule | | Why |
|---|---|---|
| `env-files` — `.env.example` names every variable with the comment that explains it (a section is a `# ---- title ----` line; an optional variable is commented, `# KEY=`). `.env` is the server's own: mode 600, never in git, never overwritten by a pull. Override: `env:`, `env_example:`. | required | `runsnip env combine` and the Env tab show each variable with its explanation; secrets never leave the server. |
| `env-generated` — Values a machine can make are declared in `env_rules.generate` (`random_hex`, `random_base64`, `literal`, `hkdf` from another secret, `ed25519_pair`, `ed25519_kid_pair`). `runsnip env generate` fills those `.env` lacks and never overwrites; a pair is filled only when both halves are empty; a derived value that differs is reported with the right one. A placeholder (`env_rules.placeholder: CHANGE_ME`) counts as empty. | default | Nobody types a secret, and a derived value cannot drift from its source. |
| `key-pairs` — An Ed25519 key pair is `<NAME>_JWT_KEY` (`"<d>.<x>"`, the JWK's private and public parts) and `<NAME>_PUBLIC_KEY` (`"<x>"`). | default | The doctor proves the halves belong together from `.env` alone. |

Values in `.env` are written as docker compose reads them: as they are when they hold only safe characters, else in
single quotes.

## 7. Data and backups

| Rule | | Why |
|---|---|---|
| `data-dir` — Data lives under `${DATA_DIR:-/srv/<compose name>}` (bind mounts and the tool alike); the checkout holds no data. Override: `data_dir:`. | default | A checkout can be deleted and cloned again; the data disk can be a mounted volume. |
| `backups` — `runsnip backup` writes each target gzipped, mode 600, into `<data>/backups`, then keeps the newest `BACKUP_KEEP` (10; 0 keeps all) of each. The default target is `pg_dump` of the service running a postgres image, as `POSTGRES_USER` on `POSTGRES_DB`. Other targets: `pg_dumpall`, or `exec` (any command in a container; its output is saved unless `save: false`; `{stamp}` is the backup's time). Backups a stack kept in `<stack>/backups` before are pruned under the same rule. Override: `backup:`, `postgres:`. `runsnip db pull` (on the operator's machine, over SSH) finds the same database from the compose labels; `database: {service, kind}` names it when the image does not (kind: postgres, mysql, mariadb). | default | Backups exist without setup, and they do not fill the disk. |

## 8. Proxies and the request shield

| Rule | | Why |
|---|---|---|
| `proxies` — A proxy's configuration is a file mounted read-only from the infra folder. The proxies are the services running caddy, nginx, traefik, haproxy or envoy (override: `proxies:`). `runsnip start` and `runsnip reload` recreate them; the doctor fails when a mounted file is newer than its container. | default | A proxy reads its config once, when its container is created: recreating is the only reload that cannot lie. |
| `shield` — Optional. A stack that puts a request shield (a forward-auth endpoint its operator runs) in front of its sites declares it in stack.yaml — `shield: {addr, addr_env, check_path, copy_headers, proxy, switch, switch_default}` — and only through Caddy: the Caddyfile defines the `(shield_on)` snippet (a `forward_auth` to `addr` + `check_path`, default `/check`) and an empty `(shield_off)`, and every site does `import shield_{$SHIELD:off}`. **Opt-in:** `SHIELD=on` in `.env` makes Caddy ask it before every request. Without the block the stack has no shield and none of this is checked. | default | The shield changes the traffic path and fails closed, so it is a decision, not a default. |

The shield is installed, configured and run by the operator, on its own; runsnip never installs, configures, runs or
inspects it. runsnip's whole part is the Caddy wiring above (`runsnip shield snippet` prints the snippet) and one
route check in the doctor: with `SHIELD=on`, it asks the address Caddy forwards to, from inside Caddy's container, and
fails with "every site returns 502" when nothing answers there.

## 9. Release, deploy branches and the shipped binaries

| Rule | | Why |
|---|---|---|
| `release` — A component is built on the build machine by `scripts/build.sh <component>…` (which also takes `--list` and `--dry-run`) and pushed to the orphan branch `deploy/<component>/v<version>`; CI turns that branch into the image `<repo>:<version>` (and `<major>.<minor>`, `latest`) and tags the source commit `<component>/v<version>`. The version is `"version"` in `<component>/package.json` (`services/<name>/package.json` for `service-<name>`). `scripts/publish-infra.sh` publishes the infra folder to the branch `infra`, one commit per publish whose subject is `infra from <sha>`. Override: `release:` (`build`, `list`, `components`, `publish_infra`, `version_files`, `paths`, `deploy_branch`, `tag`, `infra_commit`, `progress`), `infra.branch`. | default | The server never builds; every image traces back to a tag and a commit. `runsnip build` picks components, releases them and follows the CI runs. |
| `binaries` — The infra branch ships `bin/runsnip-linux-amd64` and `bin/runsnip-linux-arm64` (static, `CGO_ENABLED=0`, built with `-trimpath` and an empty build id, so the same source gives the same bytes) beside the standard `runsnip` shim, which runs the one for the machine. The publish script runs the tool's tests first and refuses a shim that differs from the standard one. | required | The server needs neither Go nor any toolchain — only `sh` and `uname`. |

## 10. The doctor contract

| Rule | | Why |
|---|---|---|
| `doctor` — `runsnip doctor` runs the standard checks and the stack's own `doctor.sections`. Each result is **pass**, **fail** (counted), **note** (true and worth knowing), **warn** (a feature is off) or **info**. A probe that fails — a container that is not running, a command that errors, even a check that panics — is reported as that check's failure, and the doctor goes on: it never stops part-way and never exits silently. It ends with the count, and exits 1 when anything failed. `--json` prints the same report as data. | required | A partial report that looks complete is worse than none. |

Standard checks, run in the stack's sections where it places them and otherwise in sections of their own: `.env`'s
mode, `.env` against the example, placeholders left, Docker, each component's running version against its pin,
every enabled service running (disabled ones say why), compose health checks, proxies running the config on disk,
the Postgres connection budget when stack.yaml names the variables (`postgres: {pool_env, url_env, reserve}`: every
pool's maximum plus the reserve within `max_connections`, read from the running containers), and, when the stack
declares a shield, its route (with `SHIELD=on`, the address Caddy forwards to answers).

A stack's own checks are typed (`runsnip validate` lists the types; the schema enumerates them): `.env` rules
(`env_set`, `env_min_len`, `env_differs`, `env_suffix`, `hkdf`, `key_pairs`, `kid_pinned`, `isolated_domains`),
probes from inside a container (`http`, `exec` with cases on its output, `logs`), from outside (`https_headers`,
`dns`), the database (`sql` returning `level|text` lines, `settings_or_env`, `ask` for an on-demand TLS endpoint),
the host (`compare`, `path_owner`, `file_present`, `sysctl`, `registry`). A section can depend on a
profile, a variable or a running container (`if:`), and say why it is off (`off:`).

## 11. Optional: the web front end and the graph

| Rule | | Why |
|---|---|---|
| `serve` — `runsnip serve` is off unless started (install.sh makes it a service). It listens on 127.0.0.1 only unless `--public`; nobody gets in without an admin's passkey or TOTP; dangerous actions need a fresh authentication; every action is a runsnip command run as a job (logged with who, when, what and how it ended, one at a time per stack); it sends a strict CSP, checks CSRF and the Origin on writes, rate-limits sign-in, keeps an append-only audit log and makes no third-party request. | default | It has the highest privilege on the server: it must not become a way in. |

`runsnip graph` draws a stack from its own files only (compose, the proxies' configuration, `.env`): nothing about
a project is known in advance. stack.yaml can adjust it: `graph: {accent, datastores, ignore}` and
`services.<name>.graph: {kind, layer, group, icon, hide}`.

`runsnip login` reads the registry account from the variable `registry.user_env` names (or the image's path).

## Adopting the standard

1. `runsnip adopt` in the project: it writes `stack.yaml.draft` beside the compose file and prints what conforms, what
   the draft overrides (and the change that drops each override) and the gaps (required rules, each with its fix).
2. Review the draft, rename it `stack.yaml`, copy `shim/runsnip` beside it, make the publish script ship the binaries.
3. `runsnip validate` until it says the stack conforms. Overrides are allowed; gaps against required rules are not.
