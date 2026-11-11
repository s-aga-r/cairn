package engine

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/s-aga-r/cairn/internal/catalog"
)

const (
	pgGlobals    = "globals.sql"
	pgDumpSuffix = ".dump"
)

// backupLogical dumps roles and tablespaces once, then every database
// in parallel in pg_dump's custom format.
func (p *Postgres) backupLogical(ctx context.Context, backup *catalog.Backup) error {
	names, err := p.databaseNames(ctx)
	if err != nil {
		return err
	}
	var lock sync.Mutex
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(p.DB.Parallel)
	dump := func(name, tool string, args ...string) {
		group.Go(func() error {
			object, err := p.Store.Upload(ctx, backup.Key(name), func(w io.Writer) error {
				cmd := p.tools.command(ctx, tool, args...)
				cmd.Stdout = w
				return run(cmd)
			})
			if err != nil {
				return err
			}
			object.Name = name
			lock.Lock()
			defer lock.Unlock()
			backup.Objects = append(backup.Objects, object)
			return nil
		})
	}
	dump(pgGlobals, "pg_dumpall", "--globals-only")
	for _, name := range names {
		// Compression is off because the upload stream compresses.
		dump(name+pgDumpSuffix, "pg_dump", "--format=custom", "--compress=0", "--dbname="+name)
	}
	err = group.Wait()
	sort.Slice(backup.Objects, func(i, j int) bool { return backup.Objects[i].Name < backup.Objects[j].Name })
	return err
}

// restoreLogical restores globals first, then recreates each database.
func (p *Postgres) restoreLogical(ctx context.Context, backup *catalog.Backup, options RestoreOptions) error {
	if err := rejectRecovery(options); err != nil {
		return err
	}
	dumps := selectDumps(backup, options.Only)
	if len(dumps) == 0 {
		return fmt.Errorf("backup %s holds no database %q", backup.ID, options.Only)
	}
	p.Log("Restoring logical backup %s (%d databases)", backup.ID, len(dumps))
	// Roles that already exist make psql report errors, which is expected.
	if err := p.feed(ctx, backup.Key(pgGlobals), "psql", "--no-psqlrc", "--quiet", "--output=/dev/null"); err != nil {
		return err
	}
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(p.DB.Parallel)
	for _, name := range dumps {
		group.Go(func() error {
			// template1 is never part of a dump, so every database, including
			// "postgres", can be dropped and recreated while connected to it.
			return p.feed(ctx, backup.Key(name), "pg_restore", "--clean", "--if-exists", "--create", "--dbname=template1")
		})
	}
	return group.Wait()
}

// feed streams a stored object into the stdin of a tool.
func (p *Postgres) feed(ctx context.Context, key, tool string, args ...string) error {
	return p.Store.Download(ctx, key, func(r io.Reader) error {
		cmd := p.tools.command(ctx, tool, args...)
		cmd.Stdin = r
		return run(cmd)
	})
}

func (p *Postgres) databaseNames(ctx context.Context) ([]string, error) {
	if len(p.DB.Databases) > 0 {
		return p.DB.Databases, nil
	}
	listing, err := p.query(ctx, "SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY 1")
	return strings.Fields(listing), err
}

// selectDumps returns the dump objects of a backup, or just the one for only.
func selectDumps(backup *catalog.Backup, only string) []string {
	var dumps []string
	for _, object := range backup.Objects {
		name, isDump := strings.CutSuffix(object.Name, pgDumpSuffix)
		if isDump && (only == "" || only == name) {
			dumps = append(dumps, object.Name)
		}
	}
	return dumps
}
