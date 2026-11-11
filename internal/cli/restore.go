package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/s-aga-r/cairn/internal/catalog"
	"github.com/s-aga-r/cairn/internal/engine"
)

type restoreFlags struct {
	backup    string
	to        string
	targetDir string
	only      string
	into      string
}

func (a *app) restoreCommand() *cobra.Command {
	var flags restoreFlags
	cmd := &cobra.Command{
		Use:   "restore [database]",
		Short: "Restore a backup from the bucket",
		Long: `Restore a backup from the bucket.

A physical backup (full, incremental) is restored into --target-dir as a
ready data directory; incrementals are merged automatically. A logical
backup is replayed into the running server from the config.

Point-in-time recovery: pass --to with a time, or "latest" to replay every
archived log. cairn picks the newest physical backup before that time.`,
		Example: `  cairn restore --target-dir /var/lib/postgresql/restore
  cairn restore main --backup 20261111-040102-123 --target-dir /srv/restore
  cairn restore main --to "2026-11-11 03:30:00" --target-dir /srv/restore
  cairn restore main --backup 20261111-050000-000 --only app --into staging`,
		Args: cobra.MaximumNArgs(1),
		RunE: a.forDatabase(func(ctx context.Context, s *session, _ []string) error {
			return s.restore(ctx, flags)
		}),
	}
	cmd.Flags().StringVarP(&flags.backup, "backup", "b", "latest", "backup ID, or latest")
	cmd.Flags().StringVar(&flags.to, "to", "", `recover to a point in time (RFC 3339 or "YYYY-MM-DD HH:MM:SS" local time), or "latest"`)
	cmd.Flags().StringVarP(&flags.targetDir, "target-dir", "d", "", "empty directory that receives a physical restore")
	cmd.Flags().StringVar(&flags.only, "only", "", "restore a single database from a logical backup")
	cmd.Flags().StringVar(&flags.into, "into", "", "replay a logical backup into another configured database server")
	return cmd
}

func (s *session) restore(ctx context.Context, flags restoreFlags) error {
	options := engine.RestoreOptions{TargetDir: flags.targetDir, Only: flags.only, Recover: flags.to != ""}
	if options.Recover && flags.to != "latest" {
		until, err := parseTime(flags.to)
		if err != nil {
			return err
		}
		options.Until = until
	}
	all, err := s.Catalog.List(ctx)
	if err != nil {
		return err
	}
	backup, err := selectBackup(all, flags.backup, options)
	if err != nil {
		return err
	}
	chain, err := all.Chain(backup.ID)
	if err != nil {
		return err
	}
	target, err := s.restoreEngine(backup, flags.into)
	if err != nil {
		return err
	}
	started := time.Now()
	if err := target.Restore(ctx, chain, options); err != nil {
		return err
	}
	s.app.printf("Restore of backup %s complete in %s\n", backup.ID, formatDuration(time.Since(started)))
	return nil
}

// restoreEngine returns the engine that receives the restore. --into
// swaps the connection for another configured server.
func (s *session) restoreEngine(backup *catalog.Backup, into string) (engine.Engine, error) {
	if into == "" {
		return s.engine, nil
	}
	if backup.Physical() {
		return nil, errors.New("--into applies to logical backups; a physical restore writes to --target-dir")
	}
	other, err := s.Config.Database(into)
	if err != nil {
		return nil, err
	}
	if other.Engine != s.DB.Engine {
		return nil, fmt.Errorf("cannot restore a %s backup into %s, which is %s", s.DB.Engine, into, other.Engine)
	}
	env := *s.Env
	env.DB = other
	return engine.New(&env), nil
}

// selectBackup finds the backup by ID, or the newest one. For recovery
// it is the newest physical backup that finished before the target time.
func selectBackup(all catalog.Set, id string, options engine.RestoreOptions) (*catalog.Backup, error) {
	if id != "latest" {
		return all.Find(id)
	}
	backup := all.Latest(func(b *catalog.Backup) bool {
		if !options.Recover {
			return true
		}
		return b.Physical() && (options.Until.IsZero() || !b.FinishedAt.After(options.Until))
	})
	if backup == nil {
		return nil, errors.New("no suitable backup found; run `cairn list` to see what is stored")
	}
	return backup, nil
}

var timeLayouts = []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"}

// parseTime accepts RFC 3339, or a date and time in the local zone.
func parseTime(text string) (time.Time, error) {
	for _, layout := range timeLayouts {
		if parsed, err := time.ParseInLocation(layout, text, time.Local); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot read time %q; use 2026-11-11T03:30:00Z, \"2026-11-11 03:30:00\" (local time), or latest", text)
}
