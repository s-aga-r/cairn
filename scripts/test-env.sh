#!/usr/bin/env bash
# Start or stop the real services the test suite runs against:
# PostgreSQL, MariaDB, and SeaweedFS as the local S3-compatible store.
#
#   scripts/test-env.sh start <dir>    start everything under <dir>
#   scripts/test-env.sh stop <dir>     stop everything and keep the files
#
# Binaries are found on PATH. Override with CAIRN_TEST_PG_BIN (directory
# with initdb and pg_ctl, PostgreSQL 17 or newer), CAIRN_TEST_MARIADB_BACKUP_BIN
# (directory with mariadb-backup and mbstream), and CAIRN_TEST_WEED.
set -euo pipefail

action="${1:?usage: test-env.sh start|stop <dir>}"
root="$(mkdir -p "${2:?usage: test-env.sh start|stop <dir>}" && cd "$2" && pwd)"

pg_bin="${CAIRN_TEST_PG_BIN:-$(dirname "$(ls /usr/lib/postgresql/*/bin/initdb | sort -V | tail -1)")}"
backup_bin="${CAIRN_TEST_MARIADB_BACKUP_BIN:-$(dirname "$(command -v mariadb-backup)")}"
weed="${CAIRN_TEST_WEED:-weed}"
s3_port="${CAIRN_TEST_S3_PORT:-18333}"
pg_port="${CAIRN_TEST_PG_PORT:-15432}"
maria_port="${CAIRN_TEST_MARIADB_PORT:-13306}"
repo="$(cd "$(dirname "$0")/.." && pwd)"

start() {
	go build -C "$repo" -o "$root/bin/cairn" ./cmd/cairn
	write_config
	start_s3
	start_postgres
	start_mariadb
	echo "Test services are up. Config: $root/cairn.json"
}

stop() {
	"$pg_bin/pg_ctl" -D "$root/pg/data" -m fast stop >/dev/null 2>&1 || true
	mariadb-admin --defaults-file="$root/mariadb/my.cnf" -uroot shutdown >/dev/null 2>&1 || true
	stop_s3
}

# stop_s3 waits for the exit, so the ports are free for the next start.
stop_s3() {
	local pid
	pid="$(cat "$root/s3/pid" 2>/dev/null || true)"
	[ -n "$pid" ] || return 0
	kill "$pid" 2>/dev/null || true
	while kill -0 "$pid" 2>/dev/null; do
		sleep 0.2
	done
}

write_config() {
	cat >"$root/cairn.json" <<JSON
{
  "storage": {
    "endpoint": "http://127.0.0.1:$s3_port",
    "bucket": "cairn-test",
    "prefix": "main",
    "access_key": "cairn",
    "secret_key": "cairn-secret",
    "part_size_mb": 5
  },
  "databases": {
    "pg": {
      "engine": "postgres",
      "socket": "$root/pg",
      "port": $pg_port,
      "user": "postgres",
      "bin_dir": "$pg_bin"
    },
    "maria": {
      "engine": "mariadb",
      "defaults_file": "$root/mariadb/my.cnf",
      "user": "cairn",
      "password": "cairn-secret",
      "bin_dir": "$backup_bin"
    }
  }
}
JSON
}

start_s3() {
	mkdir -p "$root/s3"
	cat >"$root/s3/identities.json" <<JSON
{"identities": [{"name": "cairn", "credentials": [{"accessKey": "cairn", "secretKey": "cairn-secret"}], "actions": ["Admin", "Read", "Write", "List", "Tagging"]}]}
JSON
	"$weed" server -dir="$root/s3" -ip=127.0.0.1 -ip.bind=127.0.0.1 \
		-master.port=$((s3_port + 1000)) -volume.port=$((s3_port + 2000)) -filer.port=$((s3_port + 3000)) \
		-volume.max=50 -master.volumeSizeLimitMB=128 \
		-s3 -s3.port="$s3_port" -s3.config="$root/s3/identities.json" >"$root/s3/log" 2>&1 &
	echo $! >"$root/s3/pid"
	wait_for "SeaweedFS" "$root/s3/log" sh -c \
		"echo 's3.bucket.create -name cairn-test' | '$weed' shell -master=127.0.0.1:$((s3_port + 1000)) 2>&1 | grep -q 'create bucket'"
	# The S3 gateway starts to listen after the master and filer do.
	wait_for "SeaweedFS S3 gateway" "$root/s3/log" curl -s -o /dev/null "http://127.0.0.1:$s3_port"
}

start_postgres() {
	mkdir -p "$root/pg"
	"$pg_bin/initdb" -D "$root/pg/data" -U postgres -A trust >/dev/null
	cat >>"$root/pg/data/postgresql.conf" <<CONF
port = $pg_port
listen_addresses = ''
unix_socket_directories = '$root/pg'
summarize_wal = on
archive_mode = on
archive_command = '"$root/bin/cairn" --config "$root/cairn.json" wal push pg "%p"'
CONF
	"$pg_bin/pg_ctl" -D "$root/pg/data" -l "$root/pg/log" -w start >/dev/null
}

start_mariadb() {
	mkdir -p "$root/mariadb"
	cat >"$root/mariadb/my.cnf" <<CONF
[client-server]
socket = $root/mariadb/mariadb.sock
port = $maria_port

[mysqld]
datadir = $root/mariadb/data
bind-address = 127.0.0.1
log-bin = binlog
server-id = 1
pid-file = $root/mariadb/mariadb.pid
log-error = $root/mariadb/log
CONF
	mariadb-install-db --defaults-file="$root/mariadb/my.cnf" --auth-root-authentication-method=normal >/dev/null 2>&1
	mariadbd --defaults-file="$root/mariadb/my.cnf" >/dev/null 2>&1 &
	wait_for "MariaDB" "$root/mariadb/log" mariadb --defaults-file="$root/mariadb/my.cnf" -uroot -e \
		"CREATE USER IF NOT EXISTS cairn@localhost IDENTIFIED BY 'cairn-secret'; GRANT ALL ON *.* TO cairn@localhost"
}

# wait_for <name> <log file> <probe command...>
wait_for() {
	local name="$1" log="$2"
	shift 2
	for _ in $(seq 120); do
		if "$@" >/dev/null 2>&1; then
			return 0
		fi
		sleep 0.5
	done
	echo "$name did not start. Last log lines:" >&2
	tail -20 "$log" >&2
	exit 1
}

case "$action" in
start) start ;;
stop) stop ;;
*) echo "unknown action: $action" >&2 && exit 2 ;;
esac
