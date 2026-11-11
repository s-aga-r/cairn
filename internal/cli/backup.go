package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/s-aga-r/cairn/internal/catalog"
	"github.com/s-aga-r/cairn/internal/config"
)

func (a *app) backupCommand() *cobra.Command {
	var kind string
	cmd := &cobra.Command{
		Use:   "backup [database]",
		Short: "Take a backup and store it in the bucket",
		Long: `Take a backup and store it in the bucket.

Types:
  auto         full when none exists or the last full is older than
               full_interval_hours, otherwise incremental (default)
  full         physical copy of the whole server
  incremental  physical copy of what changed since the last backup
  logical      SQL-level dump (mariadb-dump, pg_dump); portable across versions`,
		Example: "  cairn backup\n  cairn backup main --type full\n  cairn backup main -t logical",
		Args:    cobra.MaximumNArgs(1),
		RunE: a.forDatabase(func(ctx context.Context, s *session, _ []string) error {
			return s.backup(ctx, kind)
		}),
	}
	cmd.Flags().StringVarP(&kind, "type", "t", "", "backup type: "+strings.Join(config.Types, ", ")+" (default from config, else auto)")
	return cmd
}

func (s *session) backup(ctx context.Context, requested string) error {
	if requested == "" {
		requested = s.DB.DefaultType
	}
	all, err := s.Catalog.List(ctx)
	if err != nil {
		return err
	}
	kind, parent, err := s.resolveType(ctx, requested, all)
	if err != nil {
		return err
	}
	version, err := s.engine.Version(ctx)
	if err != nil {
		return err
	}
	backup := s.Catalog.New(s.DB.Engine, kind, parent, Version)
	backup.ServerVersion = version
	s.Log("Backing up %s (%s %s): %s backup %s", s.DB.Name, s.DB.Engine, version, kind, backup.ID)
	err = s.engine.Backup(ctx, backup, parent)
	if err == nil {
		err = s.Catalog.Save(ctx, backup)
	}
	if err != nil {
		// Cleanup must still run after an interrupt cancelled ctx.
		return errors.Join(err, s.Catalog.Discard(context.WithoutCancel(ctx), backup))
	}
	stored, raw := backup.Size()
	s.app.printf("Backup %s complete: %s, %s stored (%s raw) in %s\n", backup.ID, kind,
		formatBytes(stored), formatBytes(raw), formatDuration(backup.FinishedAt.Sub(backup.StartedAt)))
	return nil
}

// resolveType turns the requested type into a concrete one and picks
// the parent of an incremental.
func (s *session) resolveType(ctx context.Context, requested string, all catalog.Set) (string, *catalog.Backup, error) {
	latest := all.LatestPhysical()
	switch requested {
	case config.TypeFull, config.TypeLogical:
		return requested, nil, nil
	case config.TypeIncremental:
		if latest == nil {
			return "", nil, errors.New("an incremental backup needs a full backup first; run `cairn backup --type full`")
		}
		return requested, latest, nil
	case config.TypeAuto:
		supported, err := s.engine.SupportsIncremental(ctx)
		if err != nil {
			return "", nil, err
		}
		if latest == nil || !supported || fullDue(all, latest, time.Duration(s.DB.FullIntervalHours)*time.Hour) {
			return config.TypeFull, nil, nil
		}
		return config.TypeIncremental, latest, nil
	}
	return "", nil, fmt.Errorf("unknown backup type %q; use one of: %s", requested, strings.Join(config.Types, ", "))
}

// fullDue reports whether the chain latest belongs to is old enough,
// or broken, so that a new full backup should start a fresh chain.
func fullDue(all catalog.Set, latest *catalog.Backup, interval time.Duration) bool {
	chain, err := all.Chain(latest.ID)
	return err != nil || time.Since(chain[0].StartedAt) >= interval
}
