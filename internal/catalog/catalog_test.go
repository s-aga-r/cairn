package catalog_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/s-aga-r/cairn/internal/catalog"
	"github.com/s-aga-r/cairn/internal/config"
	"github.com/s-aga-r/cairn/internal/storage"
	"github.com/s-aga-r/cairn/internal/testenv"
)

func TestMain(m *testing.M) {
	minio.MaxRetry = 1
	os.Exit(m.Run())
}

var created int

func open(t *testing.T, edit func(map[string]any)) *catalog.Catalog {
	t.Helper()
	return testenv.Env(t, testenv.Variant(t, edit), testenv.Postgres).Catalog
}

// save stores a backup with one real object, started at the given time.
func save(t *testing.T, c *catalog.Catalog, kind string, parent *catalog.Backup, started time.Time) *catalog.Backup {
	t.Helper()
	ctx := testenv.Context(t)
	backup := c.New(config.EnginePostgres, kind, parent, "test")
	// Zero-padded, so IDs sort in creation order like real ones.
	created++
	backup.ID = fmt.Sprintf("backup-%04d", created)
	backup.StartedAt = started
	testenv.Must(t, c.Store.PutCompressed(ctx, backup.Key("data"), []byte("data")))
	backup.Objects = []storage.Object{{Name: "data", Size: 13, RawSize: 4}}
	testenv.Must(t, c.Save(ctx, backup))
	return backup
}

func ids(set catalog.Set) string {
	var out []string
	for _, backup := range set {
		out = append(out, backup.ID)
	}
	return strings.Join(out, " ")
}

func TestListAndChain(t *testing.T) {
	ctx := testenv.Context(t)
	c := open(t, nil)
	now := time.Now()
	full := save(t, c, config.TypeFull, nil, now)
	first := save(t, c, config.TypeIncremental, full, now)
	second := save(t, c, config.TypeIncremental, first, now)
	logical := save(t, c, config.TypeLogical, nil, now)

	all, err := c.List(ctx)
	testenv.Must(t, err)
	if len(all) != 4 {
		t.Fatalf("want 4 backups, got %s", ids(all))
	}
	chain, err := all.Chain(second.ID)
	if err != nil || ids(chain) != full.ID+" "+first.ID+" "+second.ID {
		t.Fatalf("unexpected chain %q, %v", ids(chain), err)
	}
	if all.LatestPhysical().ID != second.ID || all.Latest(func(*catalog.Backup) bool { return true }).ID != logical.ID {
		t.Fatal("latest picked the wrong backup")
	}
	if all.Latest(func(*catalog.Backup) bool { return false }) != nil {
		t.Fatal("latest must be nil when nothing matches")
	}
	if stored, raw := full.Size(); stored != 13 || raw != 4 {
		t.Fatalf("unexpected size %d/%d", stored, raw)
	}
	if _, err := all.Find("missing"); err == nil {
		t.Fatal("finding an unknown ID must fail")
	}

	// Discarding the chain root breaks the chain of its incrementals.
	testenv.Must(t, c.Discard(ctx, full))
	all, err = c.List(ctx)
	testenv.Must(t, err)
	if _, err := all.Chain(second.ID); err == nil || !strings.Contains(err.Error(), "broken backup chain") {
		t.Fatalf("want a broken chain, got %v", err)
	}
}

func TestListRejectsCorruptManifest(t *testing.T) {
	ctx := testenv.Context(t)
	c := open(t, nil)
	testenv.Must(t, c.Store.PutBytes(ctx, c.Database+"/backups/bad/manifest.json", []byte("{not json")))
	if _, err := c.List(ctx); err == nil || !strings.Contains(err.Error(), "unreadable manifest") {
		t.Fatalf("want an unreadable manifest, got %v", err)
	}
}

func TestPrune(t *testing.T) {
	ctx := testenv.Context(t)
	c := open(t, nil)
	past, future := time.Now().Add(-48*time.Hour), time.Now().Add(48*time.Hour)
	oldFull := save(t, c, config.TypeFull, nil, past)
	oldIncremental := save(t, c, config.TypeIncremental, oldFull, past)
	oldLogical := save(t, c, config.TypeLogical, nil, past)
	newFull := save(t, c, config.TypeFull, nil, future)
	// An incremental whose parent is gone is kept: it is never pruned blindly.
	orphan := save(t, c, config.TypeIncremental, &catalog.Backup{ID: "gone"}, future)
	newIncremental := save(t, c, config.TypeIncremental, newFull, future)
	newLogical := save(t, c, config.TypeLogical, nil, future)
	testenv.Must(t, c.Store.PutBytes(ctx, c.LogKey("000000010000000000000001"), []byte("wal")))

	all, err := c.List(ctx)
	testenv.Must(t, err)

	unlimited, err := c.Plan(ctx, all, config.Retention{})
	if err != nil || len(unlimited.Backups) != 0 || len(unlimited.Logs) != 0 {
		t.Fatalf("no retention must keep everything: %+v, %v", unlimited, err)
	}

	plan, err := c.Plan(ctx, all, config.Retention{KeepFull: 1, KeepLogical: 1})
	testenv.Must(t, err)
	if got, want := ids(plan.Backups), oldLogical.ID+" "+oldIncremental.ID+" "+oldFull.ID; got != want {
		t.Fatalf("plan removes %q, want %q", got, want)
	}
	// The newest kept physical backup starts in the future, so the log is obsolete.
	if len(plan.Logs) != 1 {
		t.Fatalf("want the archived log pruned, got %v", plan.Logs)
	}
	testenv.Must(t, c.Apply(ctx, plan))

	all, err = c.List(ctx)
	testenv.Must(t, err)
	if got, want := ids(all), strings.Join([]string{newFull.ID, orphan.ID, newIncremental.ID, newLogical.ID}, " "); got != want {
		t.Fatalf("after prune: %q, want %q", got, want)
	}
	if entries, _ := c.Store.List(ctx, c.Database+"/"); len(entries) != 8 {
		t.Fatalf("want 4 manifests and 4 objects left, got %d entries", len(entries))
	}
}

// A log that is newer than the oldest kept backup is still needed.
func TestPruneKeepsRecentLogs(t *testing.T) {
	ctx := testenv.Context(t)
	c := open(t, nil)
	save(t, c, config.TypeFull, nil, time.Now().Add(-48*time.Hour))
	save(t, c, config.TypeFull, nil, time.Now().Add(-24*time.Hour))
	testenv.Must(t, c.Store.PutBytes(ctx, c.LogKey("000000010000000000000002"), []byte("wal")))
	all, err := c.List(ctx)
	testenv.Must(t, err)
	plan, err := c.Plan(ctx, all, config.Retention{KeepFull: 1})
	if err != nil || len(plan.Backups) != 1 || len(plan.Logs) != 0 {
		t.Fatalf("unexpected plan %+v, %v", plan, err)
	}
}

func TestDeadStorage(t *testing.T) {
	ctx := testenv.Context(t)
	c := open(t, testenv.DeadEndpoint)
	backup := c.New(config.EnginePostgres, config.TypeFull, nil, "test")
	if _, err := c.List(ctx); err == nil {
		t.Fatal("list must fail")
	}
	if err := c.Discard(ctx, backup); err == nil {
		t.Fatal("discard must fail")
	}
	old := *backup
	old.ID, old.StartedAt = "old", backup.StartedAt.Add(-time.Hour)
	all := catalog.Set{old, *backup}
	if _, err := c.Plan(ctx, all, config.Retention{KeepFull: 1}); err == nil {
		t.Fatal("plan must fail when logs cannot be listed")
	}
	if err := c.Apply(ctx, catalog.Plan{Backups: all}); err == nil {
		t.Fatal("apply must fail")
	}
}
