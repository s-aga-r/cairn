package engine

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/s-aga-r/cairn/internal/catalog"
	"github.com/s-aga-r/cairn/internal/config"
)

const (
	pgBase = "base.tar"
	// pgManifest is stored on its own: the next incremental needs it
	// without downloading the whole base backup.
	pgManifest = "backup_manifest"
)

type Postgres struct {
	*Env
	tools toolbox
}

func NewPostgres(env *Env) *Postgres {
	host := env.DB.Host
	if env.DB.Socket != "" {
		host = env.DB.Socket
	}
	return &Postgres{Env: env, tools: toolbox{
		binDir: env.DB.BinDir,
		env: nonEmpty(
			"PGHOST", host,
			"PGPORT", strconv.Itoa(env.DB.Port),
			"PGUSER", env.DB.User,
			"PGPASSWORD", env.DB.Password,
			"PGDATABASE", "postgres",
			"PGCONNECT_TIMEOUT", "10",
		),
	}}
}

func (p *Postgres) MissingTools() []string {
	return p.tools.missing("psql", "pg_basebackup", "pg_combinebackup", "pg_dump", "pg_dumpall", "pg_restore", "tar")
}

func (p *Postgres) Version(ctx context.Context) (string, error) {
	return p.query(ctx, "SHOW server_version")
}

// SupportsIncremental is true on PostgreSQL 17+ with summarize_wal on.
// Older servers do not know the setting and report it as off.
func (p *Postgres) SupportsIncremental(ctx context.Context) (bool, error) {
	setting, err := p.query(ctx, "SELECT coalesce(current_setting('summarize_wal', true), 'off')")
	return setting == "on", err
}

func (p *Postgres) Backup(ctx context.Context, backup, parent *catalog.Backup) error {
	if backup.Type == config.TypeLogical {
		return p.backupLogical(ctx, backup)
	}
	return p.backupPhysical(ctx, backup, parent)
}

func (p *Postgres) Restore(ctx context.Context, chain catalog.Set, options RestoreOptions) error {
	if chain[0].Type == config.TypeLogical {
		return p.restoreLogical(ctx, &chain[0], options)
	}
	return p.restorePhysical(ctx, chain, options)
}

// backupPhysical streams a base backup as one tar that includes the WAL
// needed to make it consistent. With a parent it sends only changed blocks.
func (p *Postgres) backupPhysical(ctx context.Context, backup, parent *catalog.Backup) error {
	args := []string{"--pgdata=-", "--format=tar", "--wal-method=fetch", "--checkpoint=fast", "--no-password"}
	if parent != nil {
		manifest := filepath.Join(p.Work, pgManifest)
		if err := p.Store.DownloadFile(ctx, parent.Key(pgManifest), manifest); err != nil {
			return err
		}
		args = append(args, "--incremental="+manifest)
	}
	// The tar is teed to a second upload that keeps only the manifest.
	teeReader, tee := io.Pipe()
	manifestDone := make(chan error, 1)
	go func() {
		object, err := p.Store.Upload(ctx, backup.Key(pgManifest), func(w io.Writer) error {
			return copyTarEntry(teeReader, pgManifest, w)
		})
		teeReader.CloseWithError(err)
		object.Name = pgManifest
		backup.Objects = append(backup.Objects, object)
		manifestDone <- err
	}()
	object, err := p.Store.Upload(ctx, backup.Key(pgBase), func(w io.Writer) error {
		cmd := p.tools.command(ctx, "pg_basebackup", args...)
		cmd.Stdout = io.MultiWriter(w, tee)
		err := run(cmd)
		tee.CloseWithError(err)
		return errors.Join(err, <-manifestDone)
	})
	if err != nil {
		return err
	}
	object.Name = pgBase
	backup.Objects = append(backup.Objects, object)
	return nil
}

// restorePhysical extracts the chain. A single full backup goes straight
// into the target; a chain is staged and merged by pg_combinebackup.
func (p *Postgres) restorePhysical(ctx context.Context, chain catalog.Set, options RestoreOptions) error {
	target := options.TargetDir
	if err := requireEmptyTarget(target); err != nil {
		return err
	}
	dirs := []string{target}
	if len(chain) > 1 {
		dirs = stagingDirs(target, len(chain))
		defer removeAll(dirs)
	}
	for i := range chain {
		p.Log("Restoring %s backup %s", chain[i].Type, chain[i].ID)
		if err := p.extract(ctx, &chain[i], dirs[i]); err != nil {
			return err
		}
	}
	if len(chain) > 1 {
		p.Log("Combining %d backups", len(chain))
		combine := p.tools.command(ctx, "pg_combinebackup", append(dirs, "--output="+target)...)
		if err := run(combine); err != nil {
			return err
		}
	}
	p.Log("Data directory ready at %s", target)
	p.Log("Next: start PostgreSQL on it, for example: pg_ctl -D %s start", target)
	return p.configureRecovery(target, options)
}

func (p *Postgres) extract(ctx context.Context, backup *catalog.Backup, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return p.Store.Download(ctx, backup.Key(pgBase), func(r io.Reader) error {
		cmd := p.tools.command(ctx, "tar", "--extract", "--file=-", "--directory="+dir)
		cmd.Stdin = r
		return run(cmd)
	})
}

func (p *Postgres) query(ctx context.Context, sql string) (string, error) {
	cmd := p.tools.command(ctx, "psql", "--no-psqlrc", "--no-align", "--tuples-only", "--quiet",
		"--set=ON_ERROR_STOP=1", "--command="+sql)
	return output(cmd)
}

// copyTarEntry scans a tar stream and copies the one named entry to w.
func copyTarEntry(r io.Reader, name string, w io.Writer) error {
	reader := tar.NewReader(r)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			// Drain the padding after the end marker so the writer never blocks.
			_, err = io.Copy(io.Discard, r)
			return err
		}
		if err != nil {
			return fmt.Errorf("read backup stream: %w", err)
		}
		if header.Name != name {
			continue
		}
		if _, err := io.Copy(w, reader); err != nil {
			return err
		}
	}
}

// stagingDirs places the parts of a chain next to the target so the
// merge reads and writes one filesystem.
func stagingDirs(target string, count int) []string {
	dirs := make([]string, count)
	for i := range dirs {
		dirs[i] = fmt.Sprintf("%s.cairn-part-%d", target, i)
	}
	return dirs
}

func removeAll(dirs []string) {
	for _, dir := range dirs {
		os.RemoveAll(dir)
	}
}
