package engine_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/s-aga-r/cairn/internal/catalog"
	"github.com/s-aga-r/cairn/internal/config"
	"github.com/s-aga-r/cairn/internal/engine"
	"github.com/s-aga-r/cairn/internal/storage"
	"github.com/s-aga-r/cairn/internal/testenv"
)

const (
	pg    = testenv.Postgres
	maria = testenv.MariaDB
)

// noTime replays every archived log.
var noTime time.Time

func TestMain(m *testing.M) {
	minio.MaxRetry = 1
	os.Exit(m.Run())
}

// env builds an engine environment on a config variant.
func env(t *testing.T, database string, edit func(db map[string]any)) *engine.Env {
	t.Helper()
	return testenv.Env(t, testenv.Variant(t, func(root map[string]any) {
		if edit != nil {
			edit(testenv.Section(root, "databases", database))
		}
	}), database)
}

func wrongPort(db map[string]any) {
	delete(db, "defaults_file")
	db["socket"], db["host"], db["port"] = "", "127.0.0.1", 1
}

func failsWith(t *testing.T, want string, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want an error containing %q, got %v", want, err)
	}
}

func take(t *testing.T, e *engine.Env, kind string, parent *catalog.Backup) *catalog.Backup {
	t.Helper()
	backup := e.Catalog.New(e.DB.Engine, kind, parent, "test")
	testenv.Must(t, engine.New(e).Backup(testenv.Context(t), backup, parent))
	testenv.Must(t, e.Catalog.Save(testenv.Context(t), backup))
	return backup
}

func TestMariaDBBackupFailures(t *testing.T) {
	ctx := testenv.Context(t)
	unreachable := env(t, maria, wrongPort)
	full := unreachable.Catalog.New(config.EngineMariaDB, config.TypeFull, nil, "test")
	failsWith(t, "mariadb:", engine.New(unreachable).Backup(ctx, full, nil))

	e := env(t, maria, nil)
	incremental := e.Catalog.New(config.EngineMariaDB, config.TypeIncremental, nil, "test")
	failsWith(t, "mariadb-backup:", engine.New(e).Backup(ctx, incremental, &catalog.Backup{LSN: "not-an-lsn"}))
}

func TestMariaDBRestoreRejectsDamagedBackup(t *testing.T) {
	ctx := testenv.Context(t)
	e := env(t, maria, nil)
	damaged := e.Catalog.New(config.EngineMariaDB, config.TypeFull, nil, "test")
	testenv.Must(t, e.Store.PutCompressed(ctx, damaged.Key("backup.mbstream"), nil))
	options := engine.RestoreOptions{TargetDir: filepath.Join(t.TempDir(), "target")}
	failsWith(t, "mariadb-backup:", engine.New(e).Restore(ctx, catalog.Set{*damaged}, options))
}

func TestMariaDBBinlogFailures(t *testing.T) {
	ctx := testenv.Context(t)
	broken := env(t, maria, func(db map[string]any) {
		db["bin_dir"] = testenv.FakeTool(t, "mariadb-binlog", "echo cannot read binlog >&2; exit 1")
	})
	_, err := engine.NewMariaDB(broken).PushBinlogs(ctx)
	failsWith(t, "cannot read binlog", err)

	restored := &catalog.Backup{ID: "restored", BinlogFile: "binlog.000001", BinlogPos: "4"}
	dead := testenv.Env(t, testenv.Variant(t, testenv.DeadEndpoint), maria)
	failsWith(t, "list", engine.NewMariaDB(dead).Replay(ctx, restored, noTime))

	e := env(t, maria, nil)
	testenv.Must(t, e.Store.PutBytes(ctx, e.Catalog.LogKey("binlog.000002"), []byte("not zstd")))
	failsWith(t, "download", engine.NewMariaDB(e).Replay(ctx, restored, noTime))
}

func TestPostgresBackupFailures(t *testing.T) {
	ctx := testenv.Context(t)
	unreachable := env(t, pg, wrongPort)
	for _, kind := range []string{config.TypeFull, config.TypeLogical} {
		backup := unreachable.Catalog.New(config.EnginePostgres, kind, nil, "test")
		failsWith(t, "exit status", engine.New(unreachable).Backup(ctx, backup, nil))
	}

	e := env(t, pg, nil)
	incremental := e.Catalog.New(config.EnginePostgres, config.TypeIncremental, nil, "test")
	parent := &catalog.Backup{ID: "gone", Database: pg}
	failsWith(t, "download", engine.New(e).Backup(ctx, incremental, parent))
}

func TestPostgresRestoreFailures(t *testing.T) {
	ctx := testenv.Context(t)
	e := env(t, pg, nil)
	full := take(t, e, config.TypeFull, nil)
	incremental := take(t, e, config.TypeIncremental, full)

	broken := *e
	db := *e.DB
	db.BinDir = testenv.FakeTool(t, "pg_combinebackup", "echo cannot combine >&2; exit 1")
	broken.DB = &db
	target := filepath.Join(t.TempDir(), "target")
	err := engine.New(&broken).Restore(ctx, catalog.Set{*full, *incremental}, engine.RestoreOptions{TargetDir: target})
	failsWith(t, "cannot combine", err)
	if leftovers, _ := filepath.Glob(target + ".cairn-part-*"); len(leftovers) != 0 {
		t.Fatalf("staging directories must be removed: %v", leftovers)
	}

	// A logical backup without its globals object is incomplete.
	logical := e.Catalog.New(config.EnginePostgres, config.TypeLogical, nil, "test")
	logical.Objects = []storage.Object{{Name: "app.dump"}}
	failsWith(t, "globals.sql", engine.New(e).Restore(ctx, catalog.Set{*logical}, engine.RestoreOptions{}))
}
