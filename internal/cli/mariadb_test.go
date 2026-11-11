package cli

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/s-aga-r/cairn/internal/testenv"
)

const maria = testenv.MariaDB

func mariaDatabase(t *testing.T, rows int) string {
	t.Helper()
	name := testenv.Unique(t)
	testenv.SQL(t, maria, "mysql", "CREATE DATABASE "+name)
	t.Cleanup(func() { testenv.SQL(t, maria, "mysql", "DROP DATABASE IF EXISTS "+name) })
	testenv.SQL(t, maria, name, "CREATE TABLE items (id bigint AUTO_INCREMENT PRIMARY KEY, note text)")
	mariaInsert(t, name, rows)
	return name
}

func mariaInsert(t *testing.T, database string, rows int) {
	t.Helper()
	testenv.SQL(t, maria, database, "INSERT INTO items (note) SELECT md5(seq) FROM seq_1_to_"+itoa(rows))
}

// restoredConfig points the maria entry of a config at a restored server.
func restoredConfig(t *testing.T, config, socket string) string {
	prefix := testenv.Env(t, config, maria).Config.Storage.Prefix
	return testenv.Variant(t, func(root map[string]any) {
		testenv.Section(root, "storage")["prefix"] = prefix
		db := testenv.Section(root, "databases", maria)
		delete(db, "defaults_file")
		db["socket"] = socket
	})
}

func TestMariaDBPhysicalChain(t *testing.T) {
	config := testenv.Variant(t, nil)
	database := mariaDatabase(t, 1000)

	fullID, kind := backup(t, config, maria)
	want(t, kind, "full")
	mariaInsert(t, database, 500)
	_, kind = backup(t, config, maria)
	want(t, kind, "incremental")
	mariaInsert(t, database, 250)
	_, kind = backup(t, config, maria, "--type", "incremental")
	want(t, kind, "incremental")
	ok(t, config, "verify", maria)

	target := filepath.Join(t.TempDir(), "chain")
	ok(t, config, "restore", maria, "--target-dir", target)
	_, query := testenv.StartMariaDB(t, target)
	want(t, query(database, countItems), "1750")

	target = filepath.Join(t.TempDir(), "full")
	ok(t, config, "restore", maria, "--backup", fullID, "--target-dir", target)
	_, query = testenv.StartMariaDB(t, target)
	want(t, query(database, countItems), "1000")
}

var replayHint = regexp.MustCompile(`cairn (binlog replay maria --backup \S+ --to \S+)`)

func TestMariaDBPointInTime(t *testing.T) {
	config := testenv.Variant(t, nil)
	database := mariaDatabase(t, 100)
	backup(t, config, maria, "--type", "full")
	mariaInsert(t, database, 10)
	baseID, _ := backup(t, config, maria)

	mariaInsert(t, database, 50)
	// Binlog timestamps have one-second resolution.
	time.Sleep(1100 * time.Millisecond)
	moment := time.Now().UTC().Format(time.RFC3339)
	time.Sleep(1100 * time.Millisecond)
	mariaInsert(t, database, 25)
	if out := ok(t, config, "binlog", "push", maria); out == "Archived 0 binary logs\n" {
		t.Fatal("binlog push archived nothing")
	}
	want(t, ok(t, config, "binlog", "push", maria), "Archived 1 binary logs\n")

	target := filepath.Join(t.TempDir(), "moment")
	restored := cairn(config, "restore", maria, "--to", moment, "--target-dir", target)
	hint := replayHint.FindStringSubmatch(restored.stderr)
	if restored.code != 0 || hint == nil {
		t.Fatalf("restore should print the replay command:\n%s", restored.stderr)
	}
	socket, query := testenv.StartMariaDB(t, target)
	want(t, query(database, countItems), "110")
	ok(t, restoredConfig(t, config, socket), strings.Fields(hint[1])...)
	want(t, query(database, countItems), "160")

	target = filepath.Join(t.TempDir(), "latest")
	if out := cairn(config, "restore", maria, "--to", "latest", "--target-dir", target); strings.Contains(out.stderr, "--to") {
		t.Fatalf("replay hint for latest must not carry a time:\n%s", out.stderr)
	}
	socket, query = testenv.StartMariaDB(t, target)
	ok(t, restoredConfig(t, config, socket), "binlog", "replay", maria, "--backup", baseID)
	want(t, query(database, countItems), "185")
}

func TestMariaDBLogical(t *testing.T) {
	config := testenv.Variant(t, nil)
	database := mariaDatabase(t, 300)
	other := mariaDatabase(t, 20)
	id, kind := backup(t, config, maria, "-t", "logical")
	want(t, kind, "logical")

	testenv.SQL(t, maria, database, "DELETE FROM items")
	testenv.SQL(t, maria, other, "DELETE FROM items")
	ok(t, config, "restore", maria, "--only", database)
	want(t, testenv.SQL(t, maria, database, countItems), "300")
	want(t, testenv.SQL(t, maria, other, countItems), "0")

	ok(t, config, "restore", maria, "--backup", id)
	want(t, testenv.SQL(t, maria, other, countItems), "20")
	fails(t, "needs a physical backup", config, "restore", maria, "--backup", id, "--to", "latest")
	fails(t, "has no binlog position", config, "binlog", "replay", maria, "--backup", id)
}

func TestMariaDBSelectedDatabases(t *testing.T) {
	database := mariaDatabase(t, 40)
	config := testenv.Variant(t, func(root map[string]any) {
		testenv.Section(root, "databases", maria)["databases"] = []string{database}
	})
	backup(t, config, maria, "-t", "logical")
	testenv.SQL(t, maria, database, "DROP TABLE items")
	ok(t, config, "restore", maria)
	want(t, testenv.SQL(t, maria, database, countItems), "40")
}
