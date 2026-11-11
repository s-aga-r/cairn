package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/s-aga-r/cairn/internal/catalog"
	"github.com/s-aga-r/cairn/internal/config"
)

const (
	mariaStream = "backup.mbstream"
	mariaDump   = "dump.sql"
)

type MariaDB struct {
	*Env
	tools toolbox
}

func NewMariaDB(env *Env) *MariaDB {
	return &MariaDB{Env: env, tools: toolbox{
		binDir: env.DB.BinDir,
		// TZ keeps --stop-datetime in UTC during binlog replay.
		env: []string{"MYSQL_PWD=" + env.DB.Password, "TZ=UTC"},
	}}
}

func (m *MariaDB) MissingTools() []string {
	return m.tools.missing("mariadb", "mariadb-dump", "mariadb-backup", "mbstream", "mariadb-binlog")
}

func (m *MariaDB) Version(ctx context.Context) (string, error) {
	return m.query(ctx, "SELECT VERSION()")
}

func (m *MariaDB) SupportsIncremental(context.Context) (bool, error) {
	return true, nil
}

func (m *MariaDB) Backup(ctx context.Context, backup, parent *catalog.Backup) error {
	if backup.Type == config.TypeLogical {
		return m.backupLogical(ctx, backup)
	}
	return m.backupPhysical(ctx, backup, parent)
}

func (m *MariaDB) Restore(ctx context.Context, chain catalog.Set, options RestoreOptions) error {
	if chain[0].Type == config.TypeLogical {
		return m.restoreLogical(ctx, &chain[0], options)
	}
	return m.restorePhysical(ctx, chain, options)
}

// backupPhysical streams data files with mariadb-backup. With a parent
// it copies only pages changed since the parent's LSN.
func (m *MariaDB) backupPhysical(ctx context.Context, backup, parent *catalog.Backup) error {
	dataDir, err := m.query(ctx, "SELECT @@datadir")
	if err != nil {
		return err
	}
	lsnDir := filepath.Join(m.Work, "lsn")
	args := append(m.connArgs(), "--backup", "--stream=mbstream", "--datadir="+dataDir,
		"--extra-lsndir="+lsnDir, "--parallel="+strconv.Itoa(m.DB.Parallel))
	if parent != nil {
		args = append(args, "--incremental-lsn="+parent.LSN)
	}
	object, err := m.Store.Upload(ctx, backup.Key(mariaStream), func(w io.Writer) error {
		cmd := m.tools.command(ctx, "mariadb-backup", args...)
		cmd.Stdout = w
		return run(cmd)
	})
	if err != nil {
		return err
	}
	object.Name = mariaStream
	backup.Objects = append(backup.Objects, object)
	return recordPosition(backup, lsnDir)
}

func (m *MariaDB) backupLogical(ctx context.Context, backup *catalog.Backup) error {
	args := append(m.connArgs(), "--single-transaction", "--quick", "--routines", "--events", "--triggers", "--hex-blob")
	if len(m.DB.Databases) > 0 {
		args = append(append(args, "--databases"), m.DB.Databases...)
	} else {
		args = append(args, "--all-databases", "--flush-privileges")
	}
	object, err := m.Store.Upload(ctx, backup.Key(mariaDump), func(w io.Writer) error {
		cmd := m.tools.command(ctx, "mariadb-dump", args...)
		cmd.Stdout = w
		return run(cmd)
	})
	if err != nil {
		return err
	}
	object.Name = mariaDump
	backup.Objects = append(backup.Objects, object)
	return nil
}

// restorePhysical extracts the full backup into the target directory,
// then merges each incremental into it.
func (m *MariaDB) restorePhysical(ctx context.Context, chain catalog.Set, options RestoreOptions) error {
	target := options.TargetDir
	if err := requireEmptyTarget(target); err != nil {
		return err
	}
	for i := range chain {
		dir := target
		prepare := []string{"--prepare", "--target-dir=" + target}
		if i > 0 {
			// Incrementals stay next to the target so they share its filesystem.
			dir = fmt.Sprintf("%s.cairn-inc-%d", target, i)
			defer os.RemoveAll(dir)
			prepare = append(prepare, "--incremental-dir="+dir)
		}
		m.Log("Restoring %s backup %s", chain[i].Type, chain[i].ID)
		if err := m.extract(ctx, &chain[i], dir); err != nil {
			return err
		}
		if err := run(m.tools.command(ctx, "mariadb-backup", prepare...)); err != nil {
			return err
		}
	}
	m.Log("Data directory ready at %s", target)
	m.Log("Next: give the directory to the mysql user, point datadir at it, and start the server.")
	if options.Recover {
		m.Log("Then replay binary logs: cairn binlog replay %s --backup %s%s",
			m.DB.Name, chain[len(chain)-1].ID, untilFlag(options))
	}
	return nil
}

func (m *MariaDB) restoreLogical(ctx context.Context, backup *catalog.Backup, options RestoreOptions) error {
	if err := rejectRecovery(options); err != nil {
		return err
	}
	args := m.connArgs()
	if options.Only != "" {
		args = append(args, "--one-database", options.Only)
	}
	m.Log("Restoring logical backup %s", backup.ID)
	return m.Store.Download(ctx, backup.Key(mariaDump), func(r io.Reader) error {
		cmd := m.tools.command(ctx, "mariadb", args...)
		cmd.Stdin = r
		return run(cmd)
	})
}

func (m *MariaDB) extract(ctx context.Context, backup *catalog.Backup, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return m.Store.Download(ctx, backup.Key(mariaStream), func(r io.Reader) error {
		cmd := m.tools.command(ctx, "mbstream", "-x", "-C", dir, "--parallel="+strconv.Itoa(m.DB.Parallel))
		cmd.Stdin = r
		return run(cmd)
	})
}

func (m *MariaDB) query(ctx context.Context, sql string) (string, error) {
	args := append(m.connArgs(), "--batch", "--skip-column-names", "--execute", sql)
	return output(m.tools.command(ctx, "mariadb", args...))
}

// connArgs returns connection flags. --defaults-file must come first.
func (m *MariaDB) connArgs() []string {
	return nonEmpty(
		"--defaults-file", m.DB.DefaultsFile,
		"--socket", m.DB.Socket,
		"--host", m.DB.Host,
		"--port", strconv.Itoa(m.DB.Port),
		"--user", m.DB.User,
	)
}

var (
	lsnPattern    = regexp.MustCompile(`innodb_to_lsn = (\d+)`)
	binlogPattern = regexp.MustCompile(`binlog_pos = filename '([^']+)', position '(\d+)'`)
)

// recordPosition reads the LSN and binlog position mariadb-backup left
// in the info file. Older releases name the file xtrabackup_info.
func recordPosition(backup *catalog.Backup, lsnDir string) error {
	var info []byte
	paths, _ := filepath.Glob(filepath.Join(lsnDir, "*_info"))
	for _, path := range paths {
		data, _ := os.ReadFile(path)
		info = append(info, data...)
	}
	lsn := lsnPattern.FindSubmatch(info)
	if lsn == nil {
		return errors.New("mariadb-backup did not report the backup LSN")
	}
	backup.LSN = string(lsn[1])
	// The binlog position is absent when binary logging is off.
	if binlog := binlogPattern.FindSubmatch(info); binlog != nil {
		backup.BinlogFile, backup.BinlogPos = string(binlog[1]), string(binlog[2])
	}
	return nil
}

func untilFlag(options RestoreOptions) string {
	if options.Until.IsZero() {
		return ""
	}
	return " --to " + options.Until.UTC().Format("2006-01-02T15:04:05Z")
}
