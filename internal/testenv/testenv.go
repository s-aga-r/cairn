// Package testenv connects tests to the real services started by
// scripts/test-env.sh. It holds no product code.
package testenv

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/s-aga-r/cairn/internal/catalog"
	"github.com/s-aga-r/cairn/internal/config"
	"github.com/s-aga-r/cairn/internal/engine"
	"github.com/s-aga-r/cairn/internal/storage"
)

const (
	Postgres = "pg"
	MariaDB  = "maria"
)

var sequence atomic.Int64

// BasePath is the config written by scripts/test-env.sh. The PostgreSQL
// server archives its WAL through it.
func BasePath(t testing.TB) string {
	t.Helper()
	path := os.Getenv("CAIRN_TEST_CONFIG")
	if path == "" {
		t.Fatal("CAIRN_TEST_CONFIG is not set; run the suite with scripts/test.sh")
	}
	return path
}

// Variant writes a copy of the base config with its own storage prefix,
// so the test sees an empty catalog. edit may change the copy further.
func Variant(t testing.TB, edit func(root map[string]any)) string {
	t.Helper()
	raw, err := os.ReadFile(BasePath(t))
	Must(t, err)
	var root map[string]any
	Must(t, json.Unmarshal(raw, &root))
	Section(root, "storage")["prefix"] = Unique(t)
	if edit != nil {
		edit(root)
	}
	data, err := json.MarshalIndent(root, "", "  ")
	Must(t, err)
	path := filepath.Join(t.TempDir(), "cairn.json")
	Must(t, os.WriteFile(path, data, 0o600))
	return path
}

// Section walks nested JSON objects.
func Section(root map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		root = root[key].(map[string]any)
	}
	return root
}

// Unique returns a name that is safe as an SQL identifier and S3 prefix.
func Unique(t testing.TB) string {
	name := strings.ToLower(t.Name())
	name = strings.NewReplacer("/", "_", "-", "_", " ", "_").Replace(name)
	return fmt.Sprintf("%s_%d_%d", name, os.Getpid(), sequence.Add(1))
}

// Env builds an engine environment for one database of a config file.
func Env(t testing.TB, path, database string) *engine.Env {
	t.Helper()
	cfg, err := config.Load(path)
	Must(t, err)
	db, err := cfg.Database(database)
	Must(t, err)
	store, err := storage.Open(cfg.Storage, cfg.Compression)
	Must(t, err)
	return &engine.Env{
		Config:  cfg,
		DB:      db,
		Store:   store,
		Catalog: &catalog.Catalog{Store: store, Database: db.Name},
		Work:    t.TempDir(),
		Log:     t.Logf,
	}
}

// SQL runs a statement on the main test server of the given kind.
func SQL(t testing.TB, kind, database, sql string) string {
	t.Helper()
	db := Env(t, BasePath(t), kind).DB
	if kind == Postgres {
		return psql(t, db.BinDir, db.Socket, db.Port, database, sql)
	}
	return mariadb(t, []string{"--defaults-file=" + db.DefaultsFile, "--user=" + db.User, "--password=" + db.Password}, database, sql)
}

// StartPostgres starts a restored cluster and returns its query function.
func StartPostgres(t testing.TB, dataDir string) func(database, sql string) string {
	t.Helper()
	db := Env(t, BasePath(t), Postgres).DB
	socketDir := t.TempDir()
	port := 20000 + int(sequence.Add(1))
	options := fmt.Sprintf("-p %d -c unix_socket_directories=%s -c archive_mode=off", port, socketDir)
	pgCtl := filepath.Join(db.BinDir, "pg_ctl")
	run(t, pgCtl, "-D", dataDir, "-o", options, "-l", filepath.Join(socketDir, "log"), "-w", "-t", "120", "start")
	t.Cleanup(func() { exec.Command(pgCtl, "-D", dataDir, "-m", "immediate", "stop").Run() })
	query := func(database, sql string) string {
		return psql(t, db.BinDir, socketDir, port, database, sql)
	}
	// pg_ctl returns once read-only connections work, which is before
	// point-in-time recovery has replayed the archived WAL.
	Eventually(t, "recovery to finish", func() bool {
		return query("postgres", "SELECT pg_is_in_recovery()") == "f"
	})
	return query
}

// StartMariaDB starts a restored data directory and returns its socket
// and query function.
func StartMariaDB(t testing.TB, dataDir string) (string, func(database, sql string) string) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "s.sock")
	server := exec.Command("mariadbd", "--no-defaults", "--datadir="+dataDir, "--socket="+socket,
		"--skip-networking", "--log-error="+socket+".log", "--pid-file="+socket+".pid")
	Must(t, server.Start())
	t.Cleanup(func() {
		server.Process.Signal(syscall.SIGTERM)
		server.Wait()
	})
	args := []string{"--no-defaults", "--socket=" + socket, "--user=cairn", "--password=cairn-secret"}
	for range 240 {
		if exec.Command("mariadb", append(args, "-e", "SELECT 1")...).Run() == nil {
			return socket, func(database, sql string) string { return mariadb(t, args, database, sql) }
		}
		time.Sleep(250 * time.Millisecond)
	}
	log, _ := os.ReadFile(socket + ".log")
	t.Fatalf("restored MariaDB did not start:\n%s", log)
	return "", nil
}

// Eventually polls until check passes.
func Eventually(t testing.TB, what string, check func() bool) {
	t.Helper()
	for range 240 {
		if check() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// StoreFor opens the storage of a config file.
func StoreFor(t testing.TB, path string) *storage.Store {
	return Env(t, path, Postgres).Store
}

func Must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Context returns a context that ends with the test.
func Context(t testing.TB) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

func psql(t testing.TB, binDir, host string, port int, database, sql string) string {
	t.Helper()
	return run(t, filepath.Join(binDir, "psql"), "-h", host, "-p", fmt.Sprint(port), "-U", "postgres",
		"-d", database, "-XAtq", "-v", "ON_ERROR_STOP=1", "-c", sql)
}

func mariadb(t testing.TB, args []string, database, sql string) string {
	t.Helper()
	return run(t, "mariadb", append(args, "--batch", "--skip-column-names", "--database="+database, "-e", sql)...)
}

func run(t testing.TB, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", filepath.Base(name), args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// DeadEndpoint points a config at a port where no storage listens.
func DeadEndpoint(root map[string]any) {
	Section(root, "storage")["endpoint"] = "http://127.0.0.1:1"
}

// FakeTool writes an executable script and returns its directory, for
// use as bin_dir. It makes one external tool fail on demand.
func FakeTool(t testing.TB, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	Must(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755))
	return dir
}

// Craft stores a manifest without taking a real backup.
func Craft(t testing.TB, path string, backup catalog.Backup) {
	t.Helper()
	env := Env(t, path, backup.Database)
	Must(t, env.Catalog.Save(Context(t), &backup))
}
