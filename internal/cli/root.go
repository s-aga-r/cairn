// Package cli implements the cairn command line.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/s-aga-r/cairn/internal/catalog"
	"github.com/s-aga-r/cairn/internal/config"
	"github.com/s-aga-r/cairn/internal/engine"
	"github.com/s-aga-r/cairn/internal/storage"
)

// Version is set at build time.
var Version = "dev"

const about = `cairn backs up MariaDB and PostgreSQL to S3-compatible storage.

Backups stream from the database tools through zstd straight into the
bucket, so they need no local disk space. cairn supports full and
incremental physical backups, logical dumps, and point-in-time recovery
from archived WAL or binary logs.

Quick start:
  cairn init                 write an example cairn.json
  cairn check                test storage and database access
  cairn backup               take the best next backup (full, then incrementals)
  cairn list                 show stored backups
  cairn restore --target-dir /var/lib/restore

The database argument is optional when the config has one database.`

// Run executes the command line and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	root := (&app{stdout: stdout, stderr: stderr}).rootCommand()
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}

type app struct {
	stdout     io.Writer
	stderr     io.Writer
	configPath string
}

func (a *app) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "cairn",
		Short:         "Fast MariaDB and PostgreSQL backups to S3-compatible storage",
		Long:          about,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.PersistentFlags().StringVarP(&a.configPath, "config", "c", "",
		"config file (default: $CAIRN_CONFIG, ./cairn.json, ~/.config/cairn/config.json, /etc/cairn/config.json)")
	root.AddCommand(
		a.initCommand(), a.checkCommand(), a.backupCommand(), a.listCommand(), a.restoreCommand(),
		a.verifyCommand(), a.pruneCommand(), a.walCommand(), a.binlogCommand(),
	)
	return root
}

// session is one command's view of one configured database.
type session struct {
	*engine.Env
	app    *app
	engine engine.Engine
}

func (a *app) loadConfig() (*config.Config, error) {
	path, err := config.Locate(a.configPath)
	if err != nil {
		return nil, err
	}
	return config.Load(path)
}

func (a *app) open(name string) (*session, error) {
	cfg, err := a.loadConfig()
	if err != nil {
		return nil, err
	}
	db, err := cfg.Database(name)
	if err != nil {
		return nil, err
	}
	store, err := storage.Open(cfg.Storage, cfg.Compression)
	if err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp("", "cairn-")
	if err != nil {
		return nil, err
	}
	env := &engine.Env{
		Config:  cfg,
		DB:      db,
		Store:   store,
		Catalog: &catalog.Catalog{Store: store, Database: db.Name},
		Work:    work,
		Log:     a.logf,
	}
	return &session{Env: env, app: a, engine: engine.New(env)}, nil
}

func (s *session) close() {
	os.RemoveAll(s.Work)
}

// forDatabase adapts a session method to a cobra handler. The first
// argument, when present, names the database.
func (a *app) forDatabase(run func(ctx context.Context, s *session, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		name := ""
		if len(args) > 0 {
			name, args = args[0], args[1:]
		}
		s, err := a.open(name)
		if err != nil {
			return err
		}
		defer s.close()
		return run(cmd.Context(), s, args)
	}
}

// logf reports progress on stderr; results go to stdout.
func (a *app) logf(format string, args ...any) {
	fmt.Fprintf(a.stderr, format+"\n", args...)
}

func (a *app) printf(format string, args ...any) {
	fmt.Fprintf(a.stdout, format, args...)
}
