#!/bin/sh
# install.sh — install runsnip on a Linux server, and `runsnip serve` (its web front end) as a systemd service.
#
#   curl -fsSL https://github.com/runsnip/ops/releases/latest/download/install.sh | sudo sh
#   curl -fsSL …/install.sh | sudo sh -s -- --version v0.3.0 --listen 127.0.0.1:7070
#
# What it does, and nothing else:
#   1. downloads runsnip-linux-<arch> and SHA256SUMS from the release, and refuses a binary whose checksum differs;
#   2. installs it as /usr/local/bin/runsnip (atomically; an existing one is kept as runsnip.previous);
#   3. creates the state folder /var/lib/runsnip (mode 700: admins, jobs, the audit log, deploy keys);
#   4. installs runsnip-serve.service — listening on 127.0.0.1 only: put Caddy in front on a hostname you choose;
#   5. prints a one-time setup code (15 minutes) for the first admin, while there is none.
#
# Options:
#   --version vX.Y.Z    that release (default: the latest)
#   --listen ADDR       where serve listens (default 127.0.0.1:7070)
#   --no-service        install the command only
#   --releases URL      where releases are downloaded from (default https://github.com/runsnip/ops/releases;
#                       a mirror or a test server serves <URL>/download/<tag>/<file> and <URL>/latest/download/<file>)
set -eu

RELEASES="${RUNSNIP_RELEASES_BASE:-https://github.com/runsnip/ops/releases}"
VERSION=""
LISTEN="127.0.0.1:7070"
SERVICE=1
BIN=/usr/local/bin/runsnip
STATE=/var/lib/runsnip
UNIT=/etc/systemd/system/runsnip-serve.service

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --version) [ $# -ge 2 ] || die "--version needs a value"; VERSION="$2"; shift 2 ;;
    --listen) [ $# -ge 2 ] || die "--listen needs a value"; LISTEN="$2"; shift 2 ;;
    --no-service) SERVICE=0; shift ;;
    --releases) [ $# -ge 2 ] || die "--releases needs a value"; RELEASES="$2"; shift 2 ;;
    -h|--help) sed -n '2,22p' "$0" 2>/dev/null || true; exit 0 ;;
    *) die "unknown option $1" ;;
  esac
done

[ "$(uname -s)" = Linux ] || die "runsnip runs on Linux servers (this is $(uname -s))"
[ "$(id -u)" = 0 ] || die "run it as root (sudo sh), it installs into /usr/local/bin and /etc/systemd"
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "no runsnip build for $(uname -m)" ;;
esac
command -v sha256sum >/dev/null || die "sha256sum is missing (coreutils)"
if command -v curl >/dev/null; then fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null; then fetch() { wget -q -O "$2" "$1"; }
else die "curl or wget is needed"; fi

case "$VERSION" in
  "") BASE="$RELEASES/latest/download" ;;
  v*) BASE="$RELEASES/download/$VERSION" ;;
  *) BASE="$RELEASES/download/v$VERSION"; VERSION="v$VERSION" ;;
esac
ASSET="runsnip-linux-$ARCH"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM

say "Downloading $ASSET from $BASE"
fetch "$BASE/$ASSET" "$TMP/$ASSET" || die "could not download $BASE/$ASSET"
fetch "$BASE/SHA256SUMS" "$TMP/SHA256SUMS" || die "could not download $BASE/SHA256SUMS"
WANT="$(awk -v f="$ASSET" '$2 == f || $2 == "*" f { print $1 }' "$TMP/SHA256SUMS")"
[ -n "$WANT" ] || die "SHA256SUMS does not list $ASSET"
GOT="$(sha256sum "$TMP/$ASSET" | awk '{ print $1 }')"
[ "$WANT" = "$GOT" ] || die "checksum mismatch for $ASSET: expected $WANT, got $GOT — not installed"
chmod 755 "$TMP/$ASSET"
NEW="$("$TMP/$ASSET" --version 2>/dev/null)" || die "the downloaded binary does not run here"
say "✓ checksum verified: $NEW"

# Atomic: the new file is complete beside the old one, then renamed over it. The old one stays as runsnip.previous
# (runsnip self rollback, and the service's start guard, go back to it).
install -d -m 755 "$(dirname "$BIN")"
cp "$TMP/$ASSET" "$BIN.new"
chmod 755 "$BIN.new"
if [ -e "$BIN" ]; then ln -f "$BIN" "$BIN.previous" 2>/dev/null || cp -p "$BIN" "$BIN.previous"; fi
mv -f "$BIN.new" "$BIN"
say "✓ installed $BIN"

install -d -m 700 "$STATE"
chmod 700 "$STATE"
say "✓ state folder $STATE (mode 700)"

if [ "$SERVICE" = 1 ]; then
  command -v systemctl >/dev/null || die "systemd is needed for the service (or use --no-service)"
  cat > "$UNIT.new" <<UNIT
# Installed by runsnip's install.sh. runsnip serve: the web front end of this server. It listens on $LISTEN only;
# put Caddy in front on a hostname you choose (reverse_proxy $LISTEN).
[Unit]
Description=runsnip serve — the web front end of runsnip
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
Environment=RUNSNIP_STATE=$STATE
# The start guard: a binary that does not even run is replaced by the one it replaced.
ExecStartPre=/bin/sh -c '$BIN --version >/dev/null 2>&1 || { [ -x $BIN.previous ] && mv -f $BIN.previous $BIN && echo "runsnip: restored the previous binary"; }; true'
ExecStart=$BIN serve --listen $LISTEN
Restart=on-failure
RestartSec=2
# Jobs (deploys, updates) run in their own session and must outlive a restart of serve: only serve is stopped.
KillMode=process
NoNewPrivileges=yes
# Files runsnip writes that hold secrets (.env, keys, state) are created 600 themselves; checkouts and data folders
# must stay traversable by the containers' own users (postgres runs as uid 70 inside its bind mount).
UMask=0022

[Install]
WantedBy=multi-user.target
UNIT
  mv -f "$UNIT.new" "$UNIT"
  systemctl daemon-reload
  systemctl enable runsnip-serve.service >/dev/null 2>&1
  systemctl restart runsnip-serve.service
  i=0
  until "$BIN" self health >/dev/null 2>&1 || [ $i -ge 20 ]; do i=$((i + 1)); sleep 0.5; done
  if [ $i -ge 20 ]; then
    systemctl --no-pager status runsnip-serve.service | tail -n 20 >&2 || true
    die "runsnip serve did not answer on $LISTEN"
  fi
  say "✓ runsnip-serve.service is running on $LISTEN"
fi

if RUNSNIP_STATE="$STATE" "$BIN" admin setup-code --ttl 15m >"$TMP/code" 2>/dev/null; then
  say ""
  cat "$TMP/code"
  say "Open the page (through Caddy: https://<your ops hostname>/), enter the code and register a passkey."
  say "Later: sudo runsnip admin setup-code prints a new one while there is no admin yet."
else
  say "Admins exist already: sign in as usual (sudo runsnip admin add <name> adds one)."
fi
