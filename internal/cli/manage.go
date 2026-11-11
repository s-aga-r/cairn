package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/s-aga-r/cairn/internal/catalog"
)

func (a *app) listCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list [database]",
		Aliases: []string{"ls"},
		Short:   "List stored backups",
		Args:    cobra.MaximumNArgs(1),
		RunE: a.forDatabase(func(ctx context.Context, s *session, _ []string) error {
			return s.list(ctx, asJSON)
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the manifests as JSON")
	return cmd
}

func (a *app) verifyCommand() *cobra.Command {
	var id string
	cmd := &cobra.Command{
		Use:   "verify [database]",
		Short: "Download a backup and check its integrity",
		Long: `Download a backup and check its integrity.

Every object is read back and decompressed, which checks the zstd checksums
and the recorded sizes. It does not replace a periodic test restore.`,
		Args: cobra.MaximumNArgs(1),
		RunE: a.forDatabase(func(ctx context.Context, s *session, _ []string) error {
			return s.verify(ctx, id)
		}),
	}
	cmd.Flags().StringVarP(&id, "backup", "b", "latest", "backup ID, or latest")
	return cmd
}

func (a *app) pruneCommand() *cobra.Command {
	var dryRun bool
	var keepFull, keepLogical int
	cmd := &cobra.Command{
		Use:   "prune [database]",
		Short: "Delete backups and logs beyond the retention policy",
		Long: `Delete backups and logs beyond the retention policy.

Keeps the newest keep_full full backups with their incrementals, the
newest keep_logical logical backups, and the archived logs needed to
recover from the oldest kept physical backup.`,
		Args: cobra.MaximumNArgs(1),
		RunE: a.forDatabase(func(ctx context.Context, s *session, _ []string) error {
			retention := s.DB.Retention
			if keepFull > 0 {
				retention.KeepFull = keepFull
			}
			if keepLogical > 0 {
				retention.KeepLogical = keepLogical
			}
			all, listErr := s.Catalog.List(ctx)
			// An empty catalog yields an empty plan, so one check covers both calls.
			plan, planErr := s.Catalog.Plan(ctx, all, retention)
			if err := errors.Join(listErr, planErr); err != nil {
				return err
			}
			return s.prune(ctx, plan, dryRun)
		}),
	}
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "show what would be deleted")
	cmd.Flags().IntVar(&keepFull, "keep-full", 0, "full backups to keep (default from config)")
	cmd.Flags().IntVar(&keepLogical, "keep-logical", 0, "logical backups to keep (default from config)")
	return cmd
}

func (s *session) list(ctx context.Context, asJSON bool) error {
	all, err := s.Catalog.List(ctx)
	if err != nil {
		return err
	}
	if asJSON {
		// Marshal cannot fail for manifests. An empty list prints as [].
		data, _ := json.MarshalIndent(append(catalog.Set{}, all...), "", "  ")
		s.app.printf("%s\n", data)
		return nil
	}
	if len(all) == 0 {
		s.app.printf("No backups of %s yet. Run `cairn backup %s` to take one.\n", s.DB.Name, s.DB.Name)
		return nil
	}
	table := tabwriter.NewWriter(s.app.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "ID\tTYPE\tPARENT\tSTORED\tRAW\tTOOK\tSTARTED (UTC)")
	for _, backup := range all {
		stored, raw := backup.Size()
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", backup.ID, backup.Type, orDash(backup.Parent),
			formatBytes(stored), formatBytes(raw), formatDuration(backup.FinishedAt.Sub(backup.StartedAt)),
			backup.StartedAt.Format("2006-01-02 15:04:05"))
	}
	return table.Flush()
}

func (s *session) verify(ctx context.Context, id string) error {
	all, err := s.Catalog.List(ctx)
	if err != nil {
		return err
	}
	backup, err := selectBackup(all, id, engineDefaults)
	if err != nil {
		return err
	}
	for _, object := range backup.Objects {
		var size int64
		err := s.Store.Download(ctx, backup.Key(object.Name), func(r io.Reader) error {
			size, err = io.Copy(io.Discard, r)
			return err
		})
		if err != nil {
			return fmt.Errorf("backup %s is damaged: %w", backup.ID, err)
		}
		if size != object.RawSize {
			return fmt.Errorf("backup %s is damaged: %s holds %d bytes, manifest says %d", backup.ID, object.Name, size, object.RawSize)
		}
		s.Log("%s ok (%s)", object.Name, formatBytes(size))
	}
	s.app.printf("Backup %s verified: %d objects intact\n", backup.ID, len(backup.Objects))
	return nil
}

func (s *session) prune(ctx context.Context, plan catalog.Plan, dryRun bool) error {
	if len(plan.Backups) == 0 && len(plan.Logs) == 0 {
		s.app.printf("Nothing to prune.\n")
		return nil
	}
	verb := "Deleted"
	if dryRun {
		verb = "Would delete"
	}
	for _, backup := range plan.Backups {
		s.app.printf("%s %s backup %s\n", verb, backup.Type, backup.ID)
	}
	if len(plan.Logs) > 0 {
		s.app.printf("%s %d archived logs\n", verb, len(plan.Logs))
	}
	if dryRun {
		return nil
	}
	return s.Catalog.Apply(ctx, plan)
}
