package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/s-aga-r/cairn/internal/engine"
)

func (a *app) walCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wal",
		Short: "Archive and fetch PostgreSQL WAL (for point-in-time recovery)",
		Long: `Archive and fetch PostgreSQL WAL (for point-in-time recovery).

Enable continuous archiving in postgresql.conf:
  archive_mode = on
  archive_command = 'cairn --config /etc/cairn/config.json wal push main %p'

cairn restore --to writes the matching restore_command for you.`,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "push <database> <path>",
			Short: "Archive one WAL file (use as archive_command)",
			Args:  cobra.ExactArgs(2),
			RunE: a.forDatabase(func(ctx context.Context, s *session, args []string) error {
				postgres, err := s.postgres()
				if err != nil {
					return err
				}
				return postgres.PushWAL(ctx, args[0])
			}),
		},
		&cobra.Command{
			Use:   "fetch <database> <name> <destination>",
			Short: "Fetch one WAL file (use as restore_command)",
			Args:  cobra.ExactArgs(3),
			RunE: a.forDatabase(func(ctx context.Context, s *session, args []string) error {
				postgres, err := s.postgres()
				if err != nil {
					return err
				}
				return postgres.FetchWAL(ctx, args[0], args[1])
			}),
		},
	)
	return cmd
}

func (a *app) binlogCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "binlog",
		Short: "Archive and replay MariaDB binary logs (for point-in-time recovery)",
		Long: `Archive and replay MariaDB binary logs (for point-in-time recovery).

Run "cairn binlog push" from cron or a systemd timer. Each run rotates
the binary log and uploads the closed logs, so the interval is the most
data a recovery can lose. The server needs log_bin enabled.`,
	}
	var id, to string
	replay := &cobra.Command{
		Use:     "replay [database]",
		Short:   "Apply archived binary logs to a restored server",
		Example: `  cairn binlog replay main --backup 20261111-040102-123 --to "2026-11-11 03:30:00"`,
		Args:    cobra.MaximumNArgs(1),
		RunE: a.forDatabase(func(ctx context.Context, s *session, _ []string) error {
			return s.replay(ctx, id, to)
		}),
	}
	replay.Flags().StringVarP(&id, "backup", "b", "", "ID of the physical backup that was restored")
	replay.Flags().StringVar(&to, "to", "", "stop at this time (default: replay everything archived)")
	// Without the flag there is no start position, so cobra enforces it.
	_ = replay.MarkFlagRequired("backup")
	cmd.AddCommand(replay, &cobra.Command{
		Use:   "push [database]",
		Short: "Rotate the binary log and archive the closed logs",
		Args:  cobra.MaximumNArgs(1),
		RunE: a.forDatabase(func(ctx context.Context, s *session, _ []string) error {
			mariadb, err := s.mariadb()
			if err != nil {
				return err
			}
			pushed, err := mariadb.PushBinlogs(ctx)
			s.app.printf("Archived %d binary logs\n", pushed)
			return err
		}),
	})
	return cmd
}

func (s *session) replay(ctx context.Context, id, to string) error {
	mariadb, err := s.mariadb()
	if err != nil {
		return err
	}
	var until time.Time
	if to != "" {
		if until, err = parseTime(to); err != nil {
			return err
		}
	}
	all, err := s.Catalog.List(ctx)
	if err != nil {
		return err
	}
	backup, err := all.Find(id)
	if err != nil {
		return err
	}
	if err := mariadb.Replay(ctx, backup, until); err != nil {
		return err
	}
	s.app.printf("Binary logs replayed on top of backup %s\n", backup.ID)
	return nil
}

func (s *session) postgres() (*engine.Postgres, error) {
	postgres, ok := s.engine.(*engine.Postgres)
	if !ok {
		return nil, fmt.Errorf("%s is a %s database; WAL commands need PostgreSQL (see `cairn binlog`)", s.DB.Name, s.DB.Engine)
	}
	return postgres, nil
}

func (s *session) mariadb() (*engine.MariaDB, error) {
	mariadb, ok := s.engine.(*engine.MariaDB)
	if !ok {
		return nil, fmt.Errorf("%s is a %s database; binlog commands need MariaDB (see `cairn wal`)", s.DB.Name, s.DB.Engine)
	}
	return mariadb, nil
}
