// Package engine drives the native backup tools of each database.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/s-aga-r/cairn/internal/catalog"
	"github.com/s-aga-r/cairn/internal/config"
	"github.com/s-aga-r/cairn/internal/storage"
)

type Engine interface {
	// MissingTools lists the external programs the engine cannot find.
	MissingTools() []string
	Version(ctx context.Context) (string, error)
	SupportsIncremental(ctx context.Context) (bool, error)
	// Backup uploads the data and records the objects in backup.
	Backup(ctx context.Context, backup, parent *catalog.Backup) error
	// Restore applies a chain: one full or logical backup, then incrementals.
	Restore(ctx context.Context, chain catalog.Set, options RestoreOptions) error
}

// Env is what an engine needs to do its work.
type Env struct {
	Config  *config.Config
	DB      *config.Database
	Store   *storage.Store
	Catalog *catalog.Catalog
	// Work is a scratch directory removed when the command ends.
	Work string
	Log  func(format string, args ...any)
}

type RestoreOptions struct {
	// TargetDir receives a physical restore. It must be empty.
	TargetDir string
	// Only limits a logical restore to one database.
	Only string
	// Recover asks for log replay after the restore, up to Until.
	// A zero Until replays everything archived.
	Recover bool
	Until   time.Time
}

func New(env *Env) Engine {
	if env.DB.Engine == config.EngineMariaDB {
		return NewMariaDB(env)
	}
	return NewPostgres(env)
}

// requireEmptyTarget guards against restoring over existing data.
func requireEmptyTarget(dir string) error {
	if dir == "" {
		return errors.New("a physical restore needs --target-dir, an empty directory to restore into")
	}
	// A missing directory is fine; it is created later.
	entries, _ := os.ReadDir(dir)
	if len(entries) > 0 {
		return fmt.Errorf("target directory %s is not empty; restore never overwrites existing data", dir)
	}
	return nil
}

func rejectRecovery(options RestoreOptions) error {
	if options.Recover {
		return errors.New("point-in-time recovery needs a physical backup; logical backups cannot replay logs")
	}
	return nil
}
