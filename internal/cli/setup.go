package cli

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

//go:embed example.json
var exampleConfig []byte

func (a *app) initCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "init [path]",
		Short: "Write an example config file",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path := "cairn.json"
			if len(args) > 0 {
				path = args[0]
			}
			// O_EXCL keeps an existing config, which may hold secrets, intact.
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			_, err = file.Write(exampleConfig)
			a.printf("Wrote %s. Edit it, then run `cairn check`.\n", path)
			return errors.Join(err, file.Close())
		},
	}
}

func (a *app) checkCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "check [database]",
		Short: "Test the config, storage access, and database access",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			names := cfg.Names()
			if len(args) > 0 {
				names = args
			}
			failed := 0
			for i, name := range names {
				s, err := a.open(name)
				if err != nil {
					return err
				}
				defer s.close()
				if i == 0 {
					failed += a.report("storage "+cfg.Storage.Bucket, "reachable", s.Store.Ping(cmd.Context()))
				}
				failed += s.check(cmd.Context())
			}
			if failed > 0 {
				return fmt.Errorf("%d checks failed", failed)
			}
			return nil
		},
	}
}

// check tests one database and returns the number of failed checks.
func (s *session) check(ctx context.Context) int {
	var toolsErr error
	if missing := s.engine.MissingTools(); len(missing) > 0 {
		toolsErr = fmt.Errorf("not found: %s (install them or set bin_dir)", strings.Join(missing, ", "))
	}
	failed := s.app.report(s.DB.Name+" tools", "installed", toolsErr)
	version, err := s.engine.Version(ctx)
	failed += s.app.report(s.DB.Name+" connection", s.DB.Engine+" "+version, err)
	if err != nil {
		return failed
	}
	// The connection works, so this query cannot fail on its own.
	incremental, _ := s.engine.SupportsIncremental(ctx)
	detail := "supported"
	if !incremental {
		detail = "not available; auto takes full backups (PostgreSQL needs 17+ with summarize_wal = on)"
	}
	return failed + s.app.report(s.DB.Name+" incremental", detail, nil)
}

// report prints one check result and returns 1 when it failed.
func (a *app) report(label, detail string, err error) int {
	if err != nil {
		a.printf("FAIL  %s: %v\n", label, err)
		return 1
	}
	a.printf("ok    %s: %s\n", label, detail)
	return 0
}
