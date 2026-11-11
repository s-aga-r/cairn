package cli

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/s-aga-r/cairn/internal/testenv"
)

const pg = testenv.Postgres

// pgDatabase creates a database with one table of n rows.
func pgDatabase(t *testing.T, rows int) string {
	t.Helper()
	name := testenv.Unique(t)
	testenv.SQL(t, pg, "postgres", "CREATE DATABASE "+name)
	t.Cleanup(func() { testenv.SQL(t, pg, "postgres", "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") })
	pgInsert(t, name, rows)
	return name
}

func pgInsert(t *testing.T, database string, rows int) {
	t.Helper()
	testenv.SQL(t, pg, database, "CREATE TABLE IF NOT EXISTS items (id bigserial PRIMARY KEY, note text)")
	testenv.SQL(t, pg, database, "INSERT INTO items (note) SELECT md5(n::text) FROM generate_series(1, "+itoa(rows)+") n")
}

const countItems = "SELECT count(*) FROM items"

func TestPostgresPhysicalChain(t *testing.T) {
	config := testenv.Variant(t, nil)
	database := pgDatabase(t, 1000)

	fullID, kind := backup(t, config, pg)
	want(t, kind, "full")
	pgInsert(t, database, 500)
	_, kind = backup(t, config, pg)
	want(t, kind, "incremental")
	pgInsert(t, database, 250)
	lastID, kind := backup(t, config, pg, "--type", "incremental")
	want(t, kind, "incremental")

	listing := ok(t, config, "list", pg)
	if !strings.Contains(listing, fullID) || !strings.Contains(listing, lastID) {
		t.Fatalf("list misses backups:\n%s", listing)
	}
	if out := ok(t, config, "list", pg, "--json"); !strings.Contains(out, `"parent": "`+fullID+`"`) {
		t.Fatalf("json list misses the parent link:\n%s", out)
	}
	ok(t, config, "verify", pg)

	target := filepath.Join(t.TempDir(), "chain")
	ok(t, config, "restore", pg, "--target-dir", target)
	want(t, testenv.StartPostgres(t, target)(database, countItems), "1750")

	target = filepath.Join(t.TempDir(), "full")
	ok(t, config, "restore", pg, "--backup", fullID, "-d", target)
	want(t, testenv.StartPostgres(t, target)(database, countItems), "1000")
}

func TestPostgresPointInTime(t *testing.T) {
	// The server archives WAL through the base config, so the test uses it.
	config := testenv.BasePath(t)
	database := pgDatabase(t, 100)
	backup(t, config, pg, "--type", "full")

	pgInsert(t, database, 50)
	moment := testenv.SQL(t, pg, "postgres", `SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`)
	time.Sleep(50 * time.Millisecond)
	pgInsert(t, database, 25)
	segment := testenv.SQL(t, pg, "postgres", "SELECT pg_walfile_name(pg_switch_wal())")
	testenv.Eventually(t, "WAL archiving", func() bool {
		return testenv.SQL(t, pg, "postgres", "SELECT coalesce(last_archived_wal >= '"+segment+"', false) FROM pg_stat_archiver") == "t"
	})

	target := filepath.Join(t.TempDir(), "moment")
	ok(t, config, "restore", pg, "--to", moment, "--target-dir", target)
	want(t, testenv.StartPostgres(t, target)(database, countItems), "150")

	target = filepath.Join(t.TempDir(), "latest")
	ok(t, config, "restore", pg, "--to", "latest", "--target-dir", target)
	want(t, testenv.StartPostgres(t, target)(database, countItems), "175")
}

func TestPostgresLogical(t *testing.T) {
	database := pgDatabase(t, 300)
	other := pgDatabase(t, 20)
	config := testenv.Variant(t, func(root map[string]any) {
		databases := testenv.Section(root, "databases")
		replica := map[string]any{}
		for key, value := range testenv.Section(databases, pg) {
			replica[key] = value
		}
		databases["replica"] = replica
	})
	id, kind := backup(t, config, pg, "-t", "logical")
	want(t, kind, "logical")
	ok(t, config, "verify", pg, "--backup", id)

	testenv.SQL(t, pg, database, "DELETE FROM items")
	testenv.SQL(t, pg, other, "DELETE FROM items")
	ok(t, config, "restore", pg, "--only", database)
	want(t, testenv.SQL(t, pg, database, countItems), "300")
	want(t, testenv.SQL(t, pg, other, countItems), "0")

	ok(t, config, "restore", pg, "--into", "replica")
	want(t, testenv.SQL(t, pg, other, countItems), "20")

	fails(t, "holds no database", config, "restore", pg, "--only", "missing")
	fails(t, "needs a physical backup", config, "restore", pg, "--backup", id, "--to", "latest")
	fails(t, "no suitable backup", config, "restore", pg, "--to", "latest")
}

func TestPostgresSelectedDatabases(t *testing.T) {
	database := pgDatabase(t, 10)
	config := testenv.Variant(t, func(root map[string]any) {
		testenv.Section(root, "databases", pg)["databases"] = []string{database}
	})
	backup(t, config, pg, "-t", "logical")
	if out := ok(t, config, "list", pg, "--json"); strings.Contains(out, "postgres.dump") || !strings.Contains(out, database+".dump") {
		t.Fatalf("backup should hold only %s:\n%s", database, out)
	}
}

// Without WAL summaries the server cannot take incrementals, so auto
// falls back to full backups.
func TestPostgresAutoWithoutSummaries(t *testing.T) {
	config := testenv.Variant(t, nil)
	summarize := func(value string) {
		testenv.SQL(t, pg, "postgres", "ALTER SYSTEM SET summarize_wal = "+value)
		testenv.SQL(t, pg, "postgres", "SELECT pg_reload_conf()")
		testenv.Eventually(t, "summarize_wal = "+value, func() bool {
			return testenv.SQL(t, pg, "postgres", "SHOW summarize_wal") == value
		})
	}
	backup(t, config, pg)
	summarize("off")
	defer summarize("on")
	_, kind := backup(t, config, pg)
	want(t, kind, "full")
	if out := ok(t, config, "check", pg); !strings.Contains(out, "pg incremental: not available") {
		t.Fatalf("check should report missing incremental support:\n%s", out)
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
