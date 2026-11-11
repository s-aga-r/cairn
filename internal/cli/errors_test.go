package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s-aga-r/cairn/internal/catalog"
	"github.com/s-aga-r/cairn/internal/config"
	"github.com/s-aga-r/cairn/internal/storage"
	"github.com/s-aga-r/cairn/internal/testenv"
)

// craft stores a manifest without taking a real backup.
func craft(t *testing.T, path, database, id, kind, parent string) catalog.Backup {
	t.Helper()
	backup := catalog.Backup{ID: id, Database: database, Type: kind, Parent: parent, StartedAt: time.Now()}
	testenv.Craft(t, path, backup)
	return backup
}

func unreachable(database string) func(map[string]any) {
	return func(root map[string]any) {
		db := testenv.Section(root, "databases", database)
		delete(db, "defaults_file")
		db["socket"], db["host"], db["port"] = "", "127.0.0.1", 1
	}
}

func TestConfigProblems(t *testing.T) {
	config := testenv.Variant(t, nil)
	fails(t, "read config", filepath.Join(t.TempDir(), "missing.json"), "list")
	fails(t, "several databases are configured", config, "list")
	fails(t, `unknown database "nope"`, config, "list", "nope")
	fails(t, "storage.endpoint", testenv.Variant(t, func(root map[string]any) {
		testenv.Section(root, "storage")["endpoint"] = "http:///no-host"
	}), "list", pg)
	fails(t, "read config", filepath.Join(t.TempDir(), "missing.json"), "check")
	fails(t, "storage.endpoint", testenv.Variant(t, func(root map[string]any) {
		testenv.Section(root, "storage")["endpoint"] = "http:///no-host"
	}), "check")

	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CAIRN_CONFIG", "")
	var stderr bytes.Buffer
	if code := Run([]string{"list"}, &stderr, &stderr); code != 1 || !strings.Contains(stderr.String(), "no config file found") {
		t.Fatalf("want a missing config error, got %d: %s", code, stderr.String())
	}
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	fails(t, "no such file or directory", config, "list", pg)
}

func TestHelpAndVersion(t *testing.T) {
	if out := ok(t, "unused", "--version"); !strings.Contains(out, "cairn version "+Version) {
		t.Fatalf("unexpected version output %q", out)
	}
	if out := ok(t, "unused", "backup", "--help"); !strings.Contains(out, "incremental") {
		t.Fatalf("backup help should explain the types:\n%s", out)
	}
}

func TestInit(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("CAIRN_S3_ACCESS_KEY", "a")
	t.Setenv("CAIRN_S3_SECRET_KEY", "b")
	t.Setenv("CAIRN_PG_PASSWORD", "c")
	t.Setenv("CAIRN_MARIADB_PASSWORD", "d")
	ok(t, "unused", "init")
	ok(t, "unused", "init", "other.json")
	fails(t, "file exists", "unused", "init")
	for _, path := range []string{"cairn.json", "other.json"} {
		if _, err := config.Load(path); err != nil {
			t.Fatalf("the example config must be valid: %v", err)
		}
	}
}

func TestCheck(t *testing.T) {
	out := ok(t, testenv.Variant(t, nil), "check")
	for _, line := range []string{"ok    storage cairn-test: reachable", "ok    pg tools: installed", "ok    maria connection: mariadb", "ok    pg incremental: supported"} {
		if !strings.Contains(out, line) {
			t.Fatalf("check output misses %q:\n%s", line, out)
		}
	}
	fails(t, "FAIL  storage", testenv.Variant(t, testenv.DeadEndpoint), "check", pg)
	fails(t, "FAIL  pg connection", testenv.Variant(t, unreachable(pg)), "check", pg)
	fails(t, "1 checks failed", testenv.Variant(t, unreachable(maria)), "check")

	t.Setenv("PATH", t.TempDir())
	fails(t, "FAIL  maria tools: not found: mariadb, mariadb-dump, mariadb-binlog", testenv.Variant(t, nil), "check", maria)
}

func TestBackupProblems(t *testing.T) {
	config := testenv.Variant(t, nil)
	fails(t, `unknown backup type "weekly"`, config, "backup", pg, "--type", "weekly")
	fails(t, "needs a full backup first", config, "backup", pg, "--type", "incremental")
	fails(t, "dial tcp", testenv.Variant(t, testenv.DeadEndpoint), "backup", pg)

	offline := testenv.Variant(t, unreachable(pg))
	fails(t, "psql:", offline, "backup", pg)
	fails(t, "psql:", offline, "backup", pg, "--type", "full")
}

// A failed backup removes what it uploaded and never shows up in the list.
func TestFailedBackupLeavesNothing(t *testing.T) {
	for _, database := range []string{pg, maria} {
		config := testenv.Variant(t, func(root map[string]any) {
			testenv.Section(root, "databases", database)["databases"] = []string{"postgres", "mysql", "cairn_no_such_database"}
		})
		if r := cairn(config, "backup", database, "-t", "logical"); r.code == 0 {
			t.Fatalf("%s: dumping a missing database must fail", database)
		}
		entries, err := testenv.StoreFor(t, config).List(testenv.Context(t), "")
		if err != nil || len(entries) != 0 {
			t.Fatalf("%s: failed backup left objects behind: %+v, %v", database, entries, err)
		}
		if out := ok(t, config, "list", database); !strings.Contains(out, "No backups of "+database+" yet") {
			t.Fatalf("unexpected list output:\n%s", out)
		}
		want(t, ok(t, config, "list", database, "--json"), "[]\n")
	}
}

func TestRestoreProblems(t *testing.T) {
	config := testenv.Variant(t, nil)
	fails(t, "no suitable backup", config, "restore", pg)
	craft(t, config, pg, "20260101-000000-000", "full", "")
	craft(t, config, pg, "20260101-000000-001", "incremental", "gone")
	craft(t, config, pg, "20260101-000000-002", "logical", "")

	fails(t, "cannot read time", config, "restore", pg, "--to", "yesterday")
	fails(t, `backup "nope" not found`, config, "restore", pg, "--backup", "nope")
	fails(t, "broken backup chain", config, "restore", pg, "--backup", "20260101-000000-001")
	fails(t, "needs --target-dir", config, "restore", pg, "--backup", "20260101-000000-000")
	fails(t, "is not empty", config, "restore", pg, "--backup", "20260101-000000-000", "--target-dir", filepath.Dir(config))
	fails(t, "not a directory", config, "restore", pg, "--backup", "20260101-000000-000", "--target-dir", filepath.Join(config, "below-a-file"))
	fails(t, "--into applies to logical backups", config, "restore", pg, "--backup", "20260101-000000-000", "--into", maria)
	fails(t, `unknown database "nope"`, config, "restore", pg, "--into", "nope")
	fails(t, "cannot restore a postgres backup into maria", config, "restore", pg, "--into", maria)
	fails(t, "dial tcp", testenv.Variant(t, testenv.DeadEndpoint), "restore", pg)

	craft(t, config, maria, "20260101-000000-000", "full", "")
	fails(t, "needs --target-dir", config, "restore", maria)
	fails(t, "not a directory", config, "restore", maria, "--target-dir", filepath.Join(config, "below-a-file"))
	// A local date and time without a zone is accepted.
	fails(t, "no suitable backup", config, "restore", maria, "--to", "2020-01-01 00:00:00")
}

func TestVerifyDetectsDamage(t *testing.T) {
	ctx := testenv.Context(t)
	config := testenv.Variant(t, nil)
	fails(t, "no suitable backup", config, "verify", pg)
	backup := catalog.Backup{ID: "20260101-000000-000", Database: pg, Type: "logical",
		Objects: []storage.Object{{Name: "globals.sql", RawSize: 5}}}
	testenv.Craft(t, config, backup)
	store := testenv.StoreFor(t, config)

	testenv.Must(t, store.PutCompressed(ctx, backup.Key("globals.sql"), []byte("hello")))
	ok(t, config, "verify", pg)
	testenv.Must(t, store.PutCompressed(ctx, backup.Key("globals.sql"), []byte("hello, truncated world")))
	fails(t, "holds 22 bytes, manifest says 5", config, "verify", pg)
	testenv.Must(t, store.PutBytes(ctx, backup.Key("globals.sql"), []byte("not a zstd stream")))
	fails(t, "is damaged", config, "verify", pg)
	fails(t, "dial tcp", testenv.Variant(t, testenv.DeadEndpoint), "verify", pg)
	fails(t, "dial tcp", testenv.Variant(t, testenv.DeadEndpoint), "list", pg)
}

func TestPrune(t *testing.T) {
	ctx := testenv.Context(t)
	config := testenv.Variant(t, func(root map[string]any) {
		testenv.Section(root, "databases", pg)["retention"] = map[string]any{"keep_full": 2, "keep_logical": 2}
	})
	want(t, ok(t, config, "prune", pg), "Nothing to prune.\n")

	craft(t, config, pg, "20260101-000000-000", "full", "")
	craft(t, config, pg, "20260101-000000-001", "incremental", "20260101-000000-000")
	craft(t, config, pg, "20260101-000000-002", "logical", "")
	craft(t, config, pg, "20260101-000000-003", "logical", "")
	// The kept full starts far in the future, which makes today's log obsolete.
	kept := catalog.Backup{ID: "20260101-000000-004", Database: pg, Type: "full", StartedAt: time.Now().Add(48 * time.Hour)}
	testenv.Craft(t, config, kept)
	env := testenv.Env(t, config, pg)
	testenv.Must(t, env.Store.PutBytes(ctx, env.Catalog.LogKey("000000010000000000000001"), []byte("wal")))

	want(t, ok(t, config, "prune", pg), "Nothing to prune.\n")
	planned := ok(t, config, "prune", pg, "--keep-full", "1", "--keep-logical", "1", "--dry-run")
	want(t, planned, "Would delete logical backup 20260101-000000-002\n"+
		"Would delete incremental backup 20260101-000000-001\n"+
		"Would delete full backup 20260101-000000-000\n"+
		"Would delete 1 archived logs\n")
	if out := ok(t, config, "list", pg); !strings.Contains(out, "20260101-000000-000") {
		t.Fatalf("a dry run must delete nothing:\n%s", out)
	}
	want(t, ok(t, config, "prune", pg, "--keep-full", "1", "--keep-logical", "1"), strings.ReplaceAll(planned, "Would delete", "Deleted"))
	entries, err := env.Store.List(ctx, "")
	if err != nil || len(entries) != 2 {
		t.Fatalf("want the two kept manifests, got %+v, %v", entries, err)
	}

	dead := testenv.Variant(t, testenv.DeadEndpoint)
	fails(t, "dial tcp", dead, "prune", pg)
}

func TestWALCommands(t *testing.T) {
	config := testenv.Variant(t, nil)
	segment := filepath.Join(t.TempDir(), "000000010000000000000042")
	content := bytes.Repeat([]byte("wal record "), 4096)
	testenv.Must(t, os.WriteFile(segment, content, 0o600))
	ok(t, config, "wal", "push", pg, segment)

	fetched := filepath.Join(t.TempDir(), "fetched")
	ok(t, config, "wal", "fetch", pg, filepath.Base(segment), fetched)
	if got, _ := os.ReadFile(fetched); !bytes.Equal(got, content) {
		t.Fatal("fetched WAL differs from the pushed file")
	}
	// PostgreSQL reads a failed fetch as the end of the archive.
	fails(t, "download", config, "wal", "fetch", pg, "000000010000000000000099", fetched)
	fails(t, "no such file", config, "wal", "push", pg, segment+".missing")
	fails(t, "WAL commands need PostgreSQL", config, "wal", "push", maria, segment)
	fails(t, "WAL commands need PostgreSQL", config, "wal", "fetch", maria, "x", fetched)
	fails(t, "accepts 2 arg(s)", config, "wal", "push", pg)
}

func TestBinlogProblems(t *testing.T) {
	config := testenv.Variant(t, nil)
	fails(t, "binlog commands need MariaDB", config, "binlog", "push", pg)
	fails(t, "binlog commands need MariaDB", config, "binlog", "replay", pg, "--backup", "x")
	fails(t, `required flag(s) "backup" not set`, config, "binlog", "replay", maria)
	fails(t, "cannot read time", config, "binlog", "replay", maria, "--backup", "x", "--to", "yesterday")
	fails(t, `backup "x" not found`, config, "binlog", "replay", maria, "--backup", "x")
	fails(t, "mariadb:", testenv.Variant(t, unreachable(maria)), "binlog", "push", maria)

	dead := testenv.Variant(t, testenv.DeadEndpoint)
	fails(t, "dial tcp", dead, "binlog", "push", maria)
	fails(t, "dial tcp", dead, "binlog", "replay", maria, "--backup", "x")

	// A physical backup with a position, but nothing archived after it.
	id, _ := backup(t, config, maria, "--type", "full")
	fails(t, "no archived binary logs", config, "binlog", "replay", maria, "--backup", id)
}

func TestFormatting(t *testing.T) {
	cases := map[string]string{
		formatBytes(512):                         "512 B",
		formatBytes(1536):                        "1.5 KiB",
		formatBytes(5 << 30):                     "5.0 GiB",
		formatBytes(1 << 62):                     "4096.0 PiB",
		formatDuration(1234 * time.Millisecond):  "1.2s",
		formatDuration(90500 * time.Millisecond): "1m31s",
		orDash(""):                               "-",
		orDash("x"):                              "x",
	}
	for got, expected := range cases {
		if got != expected {
			t.Errorf("got %q, want %q", got, expected)
		}
	}
}

func TestFullIsDueForOldOrBrokenChains(t *testing.T) {
	recent := catalog.Backup{ID: "a", Type: "full", StartedAt: time.Now().Add(-time.Hour)}
	child := catalog.Backup{ID: "b", Type: "incremental", Parent: "a"}
	orphan := catalog.Backup{ID: "c", Type: "incremental", Parent: "gone"}
	all := catalog.Set{recent, child, orphan}
	if fullDue(all, &child, 24*time.Hour) {
		t.Error("a one hour old chain is not due within a day")
	}
	if !fullDue(all, &child, 30*time.Minute) {
		t.Error("a one hour old chain is due after 30 minutes")
	}
	if !fullDue(all, &orphan, 24*time.Hour) {
		t.Error("a broken chain needs a new full backup")
	}
}
