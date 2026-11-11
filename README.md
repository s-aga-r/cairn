# cairn

cairn backs up MariaDB and PostgreSQL to S3-compatible storage, and restores from it.

A cairn is a stack of stones that marks a point on a trail. A full backup is the base stone. Each incremental backup is a stone on top. The archived logs let you return to any point on the trail.

- **All backup types.** Full physical, incremental physical, logical, and point-in-time recovery.
- **No local disk.** A backup streams from the database tool through zstd into a parallel multipart upload.
- **One command.** `cairn backup` selects the best type. `cairn restore` merges a chain of backups for you.
- **Native tools.** cairn runs `mariadb-backup`, `mariadb-dump`, `pg_basebackup`, and `pg_dump`. Each backup stays readable with the standard tools.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/s-aga-r/cairn/develop/install.sh | sh
```

The script installs the latest release. To install a specific version, pass the version:

```sh
curl -fsSL https://raw.githubusercontent.com/s-aga-r/cairn/develop/install.sh | sh -s -- v1.2.0
```

Set `CAIRN_INSTALL_DIR` to change the target directory. The default is `/usr/local/bin` when it is writable, and `~/.local/bin` if not.

To build from source, run `go build ./cmd/cairn`.

## Requirements

cairn runs the client tools of the database. Install them on the host that runs cairn.

| Engine | Tools | Notes |
| --- | --- | --- |
| PostgreSQL | `psql`, `pg_basebackup`, `pg_dump`, `pg_dumpall`, `pg_restore`, `pg_combinebackup`, `tar` | Incremental backups need PostgreSQL 17 or later with `summarize_wal = on`. |
| MariaDB | `mariadb`, `mariadb-dump`, `mariadb-backup`, `mbstream`, `mariadb-binlog` | `mariadb-backup` must run on the database host. |

`cairn check` tells you which tool is missing.

## Quick start

1. Write an example config:

   ```sh
   cairn init
   ```

2. Edit `cairn.json`. Set the bucket and the database connection.

3. Test the storage access and the database access:

   ```sh
   cairn check
   ```

4. Take a backup:

   ```sh
   cairn backup
   ```

5. List the stored backups:

   ```sh
   cairn list
   ```

6. Restore the latest backup into an empty directory:

   ```sh
   cairn restore --target-dir /var/lib/restore
   ```

When the config has more than one database, name the database: `cairn backup app-postgres`.

## Backup types

| Type | PostgreSQL | MariaDB | Use |
| --- | --- | --- | --- |
| `full` | `pg_basebackup` | `mariadb-backup` | Fastest restore. Copy of the data files of the whole server. |
| `incremental` | `pg_basebackup --incremental` | `mariadb-backup --incremental-lsn` | Only the changes since the last physical backup. |
| `logical` | `pg_dump` for each database, and the globals | `mariadb-dump` | Portable between versions. Restores one database. |
| `auto` | | | The default. See below. |

`auto` takes a full backup when no full backup exists, or when the last full backup is older than `full_interval_hours`. In all other cases, `auto` takes an incremental backup. If the server cannot take incremental backups, `auto` takes a full backup.

```sh
cairn backup                   # auto
cairn backup main --type full
cairn backup main -t logical
```

A backup that fails removes its uploaded data. `cairn list` shows only complete backups.

## Restore

A physical restore writes a data directory into `--target-dir`. The directory must be empty. cairn downloads the full backup and each incremental backup, and merges them.

```sh
cairn restore main --target-dir /srv/restore
cairn restore main --backup 20261111-040102-123 --target-dir /srv/restore
```

After the restore, start the database server on the directory.

A logical restore replays the dump into the running server from the config.

```sh
cairn restore main --backup 20261111-050000-000
cairn restore main --backup 20261111-050000-000 --only app
cairn restore main --backup 20261111-050000-000 --into staging
```

`--only` restores one database. `--into` sends the restore to a different database entry in the config.

## Point-in-time recovery

Point-in-time recovery restores a physical backup and then replays the archived logs up to a time.

### PostgreSQL

1. Archive the WAL. Add these lines to `postgresql.conf` and restart the server:

   ```
   archive_mode = on
   archive_command = 'cairn --config /etc/cairn/config.json wal push main "%p"'
   ```

2. Restore to a time. Use `latest` to replay all archived WAL:

   ```sh
   cairn restore main --to "2026-11-11 03:30:00" --target-dir /srv/restore
   ```

3. Start PostgreSQL on the directory. The server gets the WAL from the bucket and stops at the time.

cairn selects the newest physical backup before the time. cairn also writes `restore_command` and the recovery target into the directory.

### MariaDB

1. Enable `log_bin` on the server.

2. Archive the binary logs on a schedule, for example each five minutes with cron:

   ```sh
   cairn binlog push main
   ```

   Each run rotates the binary log and uploads the closed logs. The interval is the most data that a recovery can lose.

3. Restore to a time:

   ```sh
   cairn restore main --to "2026-11-11 03:30:00" --target-dir /srv/restore
   ```

4. Start MariaDB on the directory.

5. Replay the binary logs. The restore command prints this command with the correct backup ID:

   ```sh
   cairn binlog replay main --backup 20261111-040102-123 --to 2026-11-11T03:30:00Z
   ```

A time without a zone is local time. A time in RFC 3339 format, such as `2026-11-11T03:30:00Z`, has its own zone.

## Verify and prune

`cairn verify` downloads a backup and decompresses each object. This checks the zstd checksums and the recorded sizes. Also do a test restore at regular intervals.

```sh
cairn verify main
cairn verify main --backup 20261111-040102-123
```

`cairn prune` applies the retention policy from the config. It keeps the newest `keep_full` full backups with their incremental backups, and the newest `keep_logical` logical backups. It also keeps the archived logs that the oldest kept physical backup needs.

```sh
cairn prune main --dry-run
cairn prune main
```

A retention value of 0 keeps all backups.

## Configuration

cairn reads one JSON file. It looks for the file in this sequence:

1. The `--config` flag
2. The `CAIRN_CONFIG` environment variable
3. `./cairn.json`
4. `~/.config/cairn/config.json`
5. `/etc/cairn/config.json`

Write `${NAME}` in a value to read it from the environment. This keeps secrets out of the file.

```json
{
  "storage": {
    "endpoint": "https://s3.example.com",
    "region": "us-east-1",
    "bucket": "db-backups",
    "prefix": "cairn",
    "access_key": "${CAIRN_S3_ACCESS_KEY}",
    "secret_key": "${CAIRN_S3_SECRET_KEY}"
  },
  "databases": {
    "main": {
      "engine": "postgres",
      "host": "127.0.0.1",
      "port": 5432,
      "user": "postgres",
      "password": "${CAIRN_PG_PASSWORD}",
      "retention": { "keep_full": 4, "keep_logical": 7 }
    }
  }
}
```

### `storage`

| Key | Default | Description |
| --- | --- | --- |
| `endpoint` | required | URL of the S3-compatible service. |
| `bucket` | required | Bucket name. The bucket must exist. |
| `region` | | Region, if the service needs one. |
| `prefix` | | Key prefix for all objects. |
| `access_key`, `secret_key` | | Credentials. If empty, cairn uses the AWS environment variables or the instance role. |
| `part_size_mb` | `64` | Size of one upload part. One object holds at most 10,000 parts, which is 640 GB compressed with the default. Increase the value for larger backups. |
| `concurrency` | `4` | Parts uploaded in parallel. Memory use is about `part_size_mb` times `concurrency` for each stream. |

### `compression`

| Key | Default | Description |
| --- | --- | --- |
| `level` | `3` | zstd level, 1 to 22. |
| `threads` | CPU count | Compression threads. |

### `databases.<name>`

| Key | Default | Description |
| --- | --- | --- |
| `engine` | required | `postgres` or `mariadb`. |
| `host`, `port`, `user`, `password` | | Connection settings. |
| `socket` | | Unix socket. For PostgreSQL, the socket directory. |
| `databases` | all | Databases in a logical backup. |
| `bin_dir` | | Directory of the database tools. cairn looks in `PATH` for a tool that is not there. |
| `defaults_file` | | MariaDB option file for the tools. |
| `default_type` | `auto` | Backup type when `--type` is not given. |
| `full_interval_hours` | `168` | Age of the last full backup at which `auto` takes a new one. |
| `parallel` | `4` | Parallel threads for `mariadb-backup`, and parallel dumps for PostgreSQL. |
| `retention.keep_full` | `0` | Full backups to keep. |
| `retention.keep_logical` | `0` | Logical backups to keep. |

## Limits

- cairn does not encrypt backups. Use the server-side encryption of the bucket.
- A PostgreSQL physical backup needs a user with the `REPLICATION` attribute.
- A PostgreSQL physical backup does not support additional tablespaces.
- A PostgreSQL logical restore drops and creates each database. Close the connections to these databases first.

## Development

The tests run against real PostgreSQL, MariaDB, and SeaweedFS processes. SeaweedFS is the local S3-compatible storage. Install PostgreSQL 17 or later, MariaDB with `mariadb-backup`, and the `weed` binary. Then run:

```sh
scripts/test.sh
```

The script starts the three services in a temporary directory and runs the tests. It fails if statement coverage is below 100%.

To keep the services running between test runs:

```sh
scripts/test-env.sh start /tmp/cairn-env
CAIRN_TEST_ROOT=/tmp/cairn-env scripts/test.sh
scripts/test-env.sh stop /tmp/cairn-env
```

`scripts/test-install.sh` tests the install script against a release that it builds and serves locally.

The default branch is `develop`. A tag that starts with `v` publishes a release.

## License

GNU Affero General Public License v3.0. See [LICENSE](LICENSE).
