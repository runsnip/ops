#!/usr/bin/env bash
# scripts/e2e-db.sh — `runsnip db` end to end against a fake server: a Linux container running sshd and the docker
# CLI (on this machine's Docker engine), which starts two compose stacks of its own — PostgreSQL and MariaDB — the
# way an existing server runs them, with no runsnip installed there.
#
#   scripts/e2e-db.sh [work-dir]        (default: a new temporary folder; everything it starts is removed at the end)
#
# It seeds both databases with known rows (values with newlines, tabs, backslashes, quotes, "\.", Unicode, NULLs,
# a schema and a table whose names need quoting, a partitioned table, an empty table, a 40 MB row), then:
#   pull over SSH → the sidecar's counts equal the seeded ones → restore into a fresh container → every table's
#   rows AND content hash equal the source's; the same for MariaDB; --keep-remote (hashes match on both sides);
#   docker through sudo as a non-root user; a stream cut short (exit 0 and exit 255) leaves no file under its name;
#   restore on the server refuses without --yes-i-am-sure and into an existing database; the tunnel (ip and
#   sidecar) answers a real client.
# Needs: docker, ssh, ssh-keygen, jq, go. Prints PASS/FAIL per check and exits 1 on any failure.
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT=$(pwd)

WORK=${1:-$(mktemp -d "${TMPDIR:-/tmp}/runsnip-e2e-db.XXXXXX")}
mkdir -p "$WORK"
WORK=$(cd "$WORK" && pwd -P)
VPS=runsnip-e2e-vps
IMG=runsnip-e2e-vps:1
PGRESTORE=runsnip-e2e-pg-restore
MYRESTORE=runsnip-e2e-my-restore
FAILS=0
pass() { printf '  PASS  %s\n' "$*"; }
fail() { printf '  FAIL  %s\n' "$*"; FAILS=$((FAILS + 1)); }
check() { local what=$1; shift; if "$@"; then pass "$what"; else fail "$what"; fi; }
step() { printf '\n== %s\n' "$*"; }

cleanup() {
  step "cleanup"
  [ -n "${TUNNEL_PID:-}" ] && kill "$TUNNEL_PID" 2>/dev/null || true
  docker exec "$VPS" sh -c 'cd /srv/e2eold && docker compose down -v -t 1; cd /srv/e2eshop && docker compose down -v -t 1' >/dev/null 2>&1 || true
  docker rm -f "$VPS" "$PGRESTORE" "$MYRESTORE" >/dev/null 2>&1 || true
  echo "  removed the fake server, its stacks and the restore containers (files stay in $WORK)"
}
trap cleanup EXIT

step "build runsnip and the fake server"
go build -o "$WORK/runsnip" .
RS="$WORK/runsnip"
mkdir -p "$WORK/vps" "$WORK/srv/e2eold" "$WORK/srv/e2eshop" "$WORK/out"
cat > "$WORK/vps/Dockerfile" <<'EOF'
FROM alpine:3.20
RUN apk add --no-cache openssh docker-cli docker-cli-compose sudo coreutils \
 && ssh-keygen -A && adduser -D -s /bin/sh ops \
 && sed -i -e 's/^root:[^:]*:/root:*:/' -e 's/^ops:[^:]*:/ops:*:/' /etc/shadow \
 && echo 'ops ALL=(root) NOPASSWD: /usr/bin/docker' > /etc/sudoers.d/ops \
 && sed -i -e '/^AllowTcpForwarding/d' -e '/^PermitRootLogin/d' -e '/^PasswordAuthentication/d' /etc/ssh/sshd_config \
 && printf 'PermitRootLogin prohibit-password\nPasswordAuthentication no\nAllowTcpForwarding local\n' >> /etc/ssh/sshd_config
CMD ["/usr/sbin/sshd", "-D", "-e"]
EOF
docker build -q -t "$IMG" "$WORK/vps" >/dev/null
rm -f "$WORK/key" "$WORK/key.pub"
ssh-keygen -q -t ed25519 -N '' -f "$WORK/key"
docker rm -f "$VPS" >/dev/null 2>&1 || true
docker run -d --name "$VPS" -p 127.0.0.1::22 -v /var/run/docker.sock:/var/run/docker.sock -v "$WORK/srv:/srv" "$IMG" >/dev/null
for u in root ops; do
  h=$([ $u = root ] && echo /root || echo /home/ops)
  docker exec -i "$VPS" sh -c "mkdir -p $h/.ssh && chmod 700 $h/.ssh && cat > $h/.ssh/authorized_keys && chmod 600 $h/.ssh/authorized_keys && chown -R $u $h/.ssh" < "$WORK/key.pub"
done
PORT=$(docker port "$VPS" 22/tcp | head -1 | sed 's/.*://')
cat > "$WORK/ssh_config" <<EOF
Host fakevps
  HostName 127.0.0.1
  Port $PORT
  User root
  IdentityFile $WORK/key
  IdentitiesOnly yes
  UserKnownHostsFile $WORK/known_hosts
  StrictHostKeyChecking accept-new
  LogLevel ERROR
EOF
export RUNSNIP_SSH="ssh -F $WORK/ssh_config"
for i in $(seq 1 30); do ssh -F "$WORK/ssh_config" -o BatchMode=yes fakevps true 2>/dev/null && break; sleep 1; done
check "ssh into the fake server" ssh -F "$WORK/ssh_config" -o BatchMode=yes fakevps 'docker version --format {{.Server.Version}} >/dev/null'
[ $FAILS -eq 0 ] || exit 1

step "an existing PostgreSQL stack and an existing MariaDB stack, started on the server"
cat > "$WORK/srv/e2eold/compose.yaml" <<'EOF'
services:
  postgres:
    image: postgres:16-alpine
    env_file: .env
    volumes: [pgdata:/var/lib/postgresql/data]
  app:
    image: alpine:3.20
    command: ["sleep", "infinity"]
volumes:
  pgdata:
EOF
printf 'POSTGRES_USER=olduser\nPOSTGRES_PASSWORD=e2e-pg-secret-%s\nPOSTGRES_DB=olddb\n' "$RANDOM$RANDOM" > "$WORK/srv/e2eold/.env"
cat > "$WORK/srv/e2eshop/compose.yaml" <<'EOF'
services:
  mariadb:
    image: mariadb:12.3
    env_file: .env
    volumes: [mydata:/var/lib/mysql]
volumes:
  mydata:
EOF
printf 'MARIADB_ROOT_PASSWORD=e2e-root-%s\nMARIADB_DATABASE=shop\nMARIADB_USER=shop\nMARIADB_PASSWORD=e2e-my-secret-%s\n' "$RANDOM" "$RANDOM" > "$WORK/srv/e2eshop/.env"
docker exec "$VPS" sh -c 'cd /srv/e2eold && docker compose up -d --quiet-pull 2>&1 | tail -1; cd /srv/e2eshop && docker compose up -d --quiet-pull 2>&1 | tail -1'
PG=e2eold-postgres-1
MY=e2eshop-mariadb-1
for i in $(seq 1 90); do docker exec $PG pg_isready -q -h 127.0.0.1 -U olduser 2>/dev/null && break; sleep 1; done
for i in $(seq 1 90); do docker exec $MY sh -c 'mariadb-admin ping -h 127.0.0.1 -uroot -p"$MARIADB_ROOT_PASSWORD" --silent' 2>/dev/null && break; sleep 1; done

docker exec -i $PG psql -q -v ON_ERROR_STOP=1 -U olduser -d olddb <<'SQL'
CREATE TABLE users (id serial PRIMARY KEY, email text NOT NULL, name text);
INSERT INTO users (email, name) SELECT 'u' || g || '@example.test', CASE WHEN g % 10 = 0 THEN NULL ELSE 'User ' || g END FROM generate_series(1, 1000) g;
CREATE TABLE projects (id serial PRIMARY KEY, owner int REFERENCES users(id), title text, body text);
INSERT INTO projects (owner, title, body) SELECT 1 + g % 1000, 'p' || g, E'line1\nline2\ttab \\ backslash ' || g FROM generate_series(1, 250) g;
INSERT INTO projects (owner, title, body) VALUES (1, 'dot', E'\\.'), (2, 'newlines', E'\n\n\n'), (3, 'unicode', 'tiếng Việt 日本語 🙂');
CREATE TABLE files (id bigserial PRIMARY KEY, project int REFERENCES projects(id), path text, content bytea);
INSERT INTO files (project, path, content) SELECT 1 + g % 253, '/f/' || g, convert_to(repeat(md5(g::text), 20), 'UTF8') FROM generate_series(1, 3000) g;
CREATE SCHEMA "Odd Schema";
CREATE TABLE "Odd Schema"."Audit Log" (id int, "note.x" text);
INSERT INTO "Odd Schema"."Audit Log" SELECT g, 'n(' || g || ').' FROM generate_series(1, 7) g;
CREATE TABLE empty_table (id int);
CREATE TABLE events (id int, at date NOT NULL) PARTITION BY RANGE (at);
CREATE TABLE events_2025 PARTITION OF events FOR VALUES FROM ('2025-01-01') TO ('2026-01-01');
CREATE TABLE events_2026 PARTITION OF events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
INSERT INTO events SELECT g, date '2025-06-01' + (g % 2) * 365 FROM generate_series(1, 400) g;
CREATE TABLE blobs (id int, data text);
INSERT INTO blobs SELECT 1, string_agg(md5(random()::text), '') FROM generate_series(1, 1250000);
SQL
docker exec -i $MY sh -c 'mariadb -uroot -p"$MARIADB_ROOT_PASSWORD" shop' <<'SQL'
CREATE TABLE customers (id int PRIMARY KEY, name varchar(100), note text);
INSERT INTO customers SELECT seq, CONCAT('Customer ', seq), CASE WHEN seq % 7 = 0 THEN NULL ELSE CONCAT('it''s (', seq, '); \\ "q"\nnext') END FROM seq_1_to_500;
CREATE TABLE orders (id int PRIMARY KEY, customer int, total decimal(10,2), raw blob);
INSERT INTO orders SELECT seq, 1 + seq % 500, seq * 1.5, UNHEX(MD5(seq)) FROM seq_1_to_1200;
CREATE TABLE `odd``name` (id int, v varchar(10));
INSERT INTO `odd``name` VALUES (1, ')('), (2, ';');
CREATE TABLE empty_one (id int);
SQL
pass "seeded: PostgreSQL users 1000 · projects 253 · files 3000 · \"Odd Schema\".\"Audit Log\" 7 · empty_table 0 · events_2025/2026 200/200 · blobs 1 (40 MB); MariaDB customers 500 · orders 1200 · odd\`name 2 · empty_one 0"

# hashes prints every table's rows and an md5 of its content (rows ordered), for comparing two databases.
pg_hashes() { # container user db
  docker exec -i "$1" psql -XqtA -U "$2" -d "$3" <<'SQL'
SELECT format('SELECT %L || '' '' || count(*) || '' '' || coalesce(md5(string_agg(t::text, '','' ORDER BY t::text)), ''-'') FROM %I.%I t;', quote_ident(n.nspname) || '.' || quote_ident(c.relname), n.nspname, c.relname)
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind = 'r' AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%' ORDER BY 1 \gexec
SQL
}
my_hashes() { # container db
  docker exec -i "$1" sh -c "mariadb -uroot -p\"\$MARIADB_ROOT_PASSWORD\" -NB $2" <<'SQL'
SELECT CONCAT('SELECT ', QUOTE(table_name), ', COUNT(*), COALESCE(MD5(GROUP_CONCAT(MD5(CONCAT_WS(0x1f, ', (SELECT GROUP_CONCAT(CONCAT('HEX(`', REPLACE(column_name, '`', '``'), '`)') ORDER BY ordinal_position) FROM information_schema.columns c WHERE c.table_schema = t.table_schema AND c.table_name = t.table_name), ')) ORDER BY 1 SEPARATOR '','')), ''-'') FROM `', REPLACE(table_name, '`', '``'), '`;')
  FROM information_schema.tables t WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE' ORDER BY table_name
SQL
}
my_run_hashes() { my_hashes "$1" "$2" | docker exec -i "$1" sh -c "mariadb -uroot -p\"\$MARIADB_ROOT_PASSWORD\" -NB $2" | tr '\t' ' '; }

step "db pull over SSH (PostgreSQL), from the compose folder"
uptime
set +e
/usr/bin/time -p "$RS" db pull fakevps --compose /srv/e2eold -o "$WORK/out/old.dump" > "$WORK/out/pull-pg.log" 2> "$WORK/out/pull-pg.err"
code=$?
set -e
cat "$WORK/out/pull-pg.log"; grep real "$WORK/out/pull-pg.err" || true
check "pull exits 0" test $code -eq 0
check "the dump and its sidecar exist, no .part" test -s "$WORK/out/old.dump" -a -s "$WORK/out/old.dump.json" -a ! -e "$WORK/out/old.dump.part"
SC="$WORK/out/old.dump.json"
expect_rows() { [ "$(jq -r --arg t "$1" '.dump_rows[$t]' "$SC")" = "$2" ]; }
for pair in "public.users 1000" "public.projects 253" "public.files 3000" '"Odd Schema"."Audit Log" 7' "public.empty_table 0" "public.events_2025 200" "public.events_2026 200" "public.blobs 1"; do
  t=${pair% *}; n=${pair##* }
  check "sidecar: $t has $n rows in the dump" expect_rows "$t" "$n"
done
check "sidecar: 9 tables on the server and in the dump, verified" test "$(jq -r '"\(.server_tables) \(.dump_tables) \(.verified)"' "$SC")" = "9 9 true"
SUM=$(shasum -a 256 "$WORK/out/old.dump" | cut -d' ' -f1)
check "sidecar: sha256 is the file's ($SUM)" test -n "$SUM" -a "$(jq -r .sha256 "$SC")" = "$SUM"
check "sidecar: no password in it" bash -c "test -s '$SC' && ! grep -q 'e2e-pg-secret' '$SC' '$WORK/out/pull-pg.log' '$WORK/out/pull-pg.err'"
check "nothing written on the server" bash -c "! ssh -F '$WORK/ssh_config' fakevps 'ls ~/runsnip-backups' 2>/dev/null | grep -q ."

step "db restore into a fresh local container, then compare content"
docker rm -f "$PGRESTORE" >/dev/null 2>&1 || true
set +e
"$RS" db restore "$WORK/out/old.dump" --into "$PGRESTORE" > "$WORK/out/restore-pg.log" 2>&1
code=$?
set -e
cat "$WORK/out/restore-pg.log"
check "restore exits 0 (every count equal)" test $code -eq 0
pg_hashes $PG olduser olddb | sort > "$WORK/out/pg-source.txt"
pg_hashes $PGRESTORE restore olddb | sort > "$WORK/out/pg-restored.txt"
echo "  source:";   sed 's/^/    /' "$WORK/out/pg-source.txt"
echo "  restored:"; sed 's/^/    /' "$WORK/out/pg-restored.txt"
check "every table: same rows and same content md5 as the source" diff -q "$WORK/out/pg-source.txt" "$WORK/out/pg-restored.txt"
check "db verify re-reads the dump against its sidecar" "$RS" db verify "$WORK/out/old.dump"

step "the dump never gets its name when the stream is cut"
cat > "$WORK/cut-ssh" <<EOF
#!/bin/sh
# ssh, but the stream of a pg_dump is cut after 1 MB; the exit status is CUT_STATUS.
case "\$*" in *"pg_dump -Fc"*) ssh -F "$WORK/ssh_config" "\$@" | head -c 1048576; exit \${CUT_STATUS:-0} ;; esac
exec ssh -F "$WORK/ssh_config" "\$@"
EOF
chmod +x "$WORK/cut-ssh"
for st in 0 255; do
  rm -f "$WORK/out/cut.dump"*
  set +e
  RUNSNIP_SSH="$WORK/cut-ssh" CUT_STATUS=$st "$RS" db pull fakevps --compose /srv/e2eold -o "$WORK/out/cut.dump" > "$WORK/out/cut-$st.log" 2>&1
  code=$?
  set -e
  tail -2 "$WORK/out/cut-$st.log" | sed 's/^/    /'
  check "cut stream, ssh exit $st: pull fails" test $code -ne 0
  check "cut stream, ssh exit $st: no cut.dump, no .part" test ! -e "$WORK/out/cut.dump" -a ! -e "$WORK/out/cut.dump.part"
done

step "--keep-remote, and docker through sudo as a user outside the docker group"
set +e
"$RS" db pull ops@fakevps --sudo --stack e2eold --service postgres --keep-remote -o "$WORK/out/kept.dump" > "$WORK/out/kept.log" 2>&1
code=$?
set -e
grep -E "server's copy|✓" "$WORK/out/kept.log" | sed 's/^/    /'
check "keep-remote + sudo pull exits 0" test $code -eq 0
remote=$(ssh -F "$WORK/ssh_config" ops@fakevps 'stat -c "%a %s" ~/runsnip-backups/kept.dump' 2>/dev/null || true)
check "the server's copy is mode 600 and as big as the local one" test "$remote" = "600 $(wc -c < "$WORK/out/kept.dump" | tr -d ' ')"
check "same rows as the first pull" test "$(jq -c .dump_rows "$WORK/out/kept.dump.json")" = "$(jq -c .dump_rows "$SC")"

step "db restore on the server is strict"
set +e
"$RS" db restore "$WORK/out/old.dump" --to-server fakevps --compose /srv/e2eold --db olddb_rehearsal > "$WORK/out/srv1.log" 2>&1; c1=$?
"$RS" db restore "$WORK/out/old.dump" --to-server fakevps --compose /srv/e2eold --db olddb --yes-i-am-sure > "$WORK/out/srv2.log" 2>&1; c2=$?
"$RS" db restore "$WORK/out/old.dump" --to-server fakevps --compose /srv/e2eold --db olddb_rehearsal --yes-i-am-sure > "$WORK/out/srv3.log" 2>&1; c3=$?
set -e
check "refused without --yes-i-am-sure" test $c1 -eq 2
check "refused into the existing database olddb" bash -c "test $c2 -ne 0 && grep -q 'exists' '$WORK/out/srv2.log'"
check "restored into the new database olddb_rehearsal on the server" test $c3 -eq 0
check "the original database is untouched" diff -q "$WORK/out/pg-source.txt" <(pg_hashes $PG olduser olddb | sort)

step "db tunnel (via the container's address, then via a socat sidecar), used by a real client"
# The fake server is a container: it reaches the stack's network only once joined to it (a real host reaches its
# bridge networks). Before that, the address tunnel must say so instead of opening a dead port.
set +e
"$RS" db tunnel fakevps --compose /srv/e2eold --via ip --local-port 25432 > "$WORK/out/tunnel-unreachable.log" 2>&1
code=$?
set -e
tail -1 "$WORK/out/tunnel-unreachable.log" | sed 's/^/    /'
check "tunnel --via ip to an unreachable address fails and suggests the sidecar" bash -c "test $code -ne 0 && grep -q 'via sidecar' '$WORK/out/tunnel-unreachable.log'"
docker network connect e2eold_default "$VPS"
for via in ip sidecar; do
  "$RS" db tunnel fakevps --compose /srv/e2eold --via $via --local-port 25432 > "$WORK/out/tunnel-$via.log" 2>&1 &
  TUNNEL_PID=$!
  for i in $(seq 1 60); do grep -q '✓' "$WORK/out/tunnel-$via.log" 2>/dev/null && break; sleep 1; done
  sed 's/^/    /' "$WORK/out/tunnel-$via.log"
  PW=$(sed -n 's/^POSTGRES_PASSWORD=//p' "$WORK/srv/e2eold/.env")
  n=$(docker run --rm -e PGPASSWORD="$PW" postgres:16-alpine psql -XtA -h host.docker.internal -p 25432 -U olduser -d olddb -c 'SELECT count(*) FROM users' 2>&1 || true)
  echo "    psql through 127.0.0.1:25432: $n"
  check "tunnel --via $via: psql through it counts 1000 users" test "$n" = "1000"
  kill -INT $TUNNEL_PID 2>/dev/null || true
  for i in $(seq 1 15); do kill -0 $TUNNEL_PID 2>/dev/null || break; sleep 1; done
  if kill -0 $TUNNEL_PID 2>/dev/null; then fail "tunnel --via $via: still running after Ctrl-C"; kill -9 $TUNNEL_PID; else pass "tunnel --via $via: Ctrl-C closes it"; fi
  TUNNEL_PID=
done
check "the tunnel published no port on the server" bash -c "! docker ps --format '{{.Ports}}' --filter name=e2eold | grep -q '0.0.0.0\|127.0.0.1'"

step "MariaDB: pull over SSH, restore, compare"
set +e
"$RS" db pull fakevps --stack e2eshop -o "$WORK/out/shop.sql.gz" > "$WORK/out/pull-my.log" 2>&1
code=$?
set -e
cat "$WORK/out/pull-my.log"
check "mariadb pull exits 0" test $code -eq 0
SC="$WORK/out/shop.sql.gz.json"
for pair in "customers 500" "orders 1200" 'odd`name 2' "empty_one 0"; do
  t=${pair% *}; n=${pair##* }
  check "sidecar: $t has $n rows" bash -c "test \"\$(jq -r --arg t '$t' '.dump_rows[\$t] // 0' '$SC')\" = $n"
done
check "no MariaDB password in the log or the sidecar" bash -c "test -s '$SC' && ! grep -q 'e2e-my-secret\|e2e-root' '$SC' '$WORK/out/pull-my.log'"
docker rm -f "$MYRESTORE" >/dev/null 2>&1 || true
set +e
"$RS" db restore "$WORK/out/shop.sql.gz" --into "$MYRESTORE" > "$WORK/out/restore-my.log" 2>&1
code=$?
set -e
cat "$WORK/out/restore-my.log"
check "mariadb restore exits 0" test $code -eq 0
my_run_hashes $MY shop | sort > "$WORK/out/my-source.txt"
my_run_hashes $MYRESTORE shop | sort > "$WORK/out/my-restored.txt"
echo "  source:";   sed 's/^/    /' "$WORK/out/my-source.txt"
echo "  restored:"; sed 's/^/    /' "$WORK/out/my-restored.txt"
check "every MariaDB table: same rows and same content md5" diff -q "$WORK/out/my-source.txt" "$WORK/out/my-restored.txt"

step "result"
if [ $FAILS -eq 0 ]; then echo "  all checks passed ($WORK)"; else echo "  $FAILS check(s) failed ($WORK)"; exit 1; fi
