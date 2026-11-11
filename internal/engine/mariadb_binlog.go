package engine

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/s-aga-r/cairn/internal/catalog"
	"github.com/s-aga-r/cairn/internal/storage"
)

// PushBinlogs rotates the binary log and archives every closed log that
// is not in the bucket yet. It returns the number of logs uploaded.
func (m *MariaDB) PushBinlogs(ctx context.Context) (int, error) {
	listing, err := m.query(ctx, "FLUSH BINARY LOGS; SHOW BINARY LOGS")
	if err != nil {
		return 0, err
	}
	archived, err := m.Store.List(ctx, m.Catalog.LogPrefix())
	if err != nil {
		return 0, err
	}
	have := map[string]bool{}
	for _, entry := range archived {
		have[entry.Key] = true
	}
	pushed := 0
	// The last log is still being written, so it stays.
	for _, name := range closedBinlogs(listing) {
		if have[m.Catalog.LogKey(name)] {
			continue
		}
		if err := m.pushBinlog(ctx, name); err != nil {
			return pushed, err
		}
		pushed++
	}
	return pushed, nil
}

// Replay applies archived binary logs to the running server, from the
// position of a restored backup up to until. A zero until applies all.
func (m *MariaDB) Replay(ctx context.Context, backup *catalog.Backup, until time.Time) error {
	if backup.BinlogFile == "" {
		return fmt.Errorf("backup %s has no binlog position; it must be a physical backup taken with log_bin enabled", backup.ID)
	}
	files, err := m.fetchBinlogs(ctx, backup.BinlogFile)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no archived binary logs from %s on; run `cairn binlog push %s` on the source first",
			backup.BinlogFile, m.DB.Name)
	}
	args := []string{"--start-position=" + backup.BinlogPos}
	if !until.IsZero() {
		args = append(args, "--stop-datetime="+until.UTC().Format("2006-01-02 15:04:05"))
	}
	m.Log("Replaying %d binary logs from %s:%s", len(files), backup.BinlogFile, backup.BinlogPos)
	decode := m.tools.command(ctx, "mariadb-binlog", append(args, files...)...)
	apply := m.tools.command(ctx, "mariadb", m.connArgs()...)
	return pipe(decode, apply)
}

// pushBinlog copies one log through the replication protocol, so it
// works without file access to the server.
func (m *MariaDB) pushBinlog(ctx context.Context, name string) error {
	args := append(m.connArgs(), "--read-from-remote-server", "--raw", "--result-file="+m.Work+"/", name)
	if err := run(m.tools.command(ctx, "mariadb-binlog", args...)); err != nil {
		return err
	}
	_, err := m.Store.UploadFile(ctx, m.Catalog.LogKey(name), filepath.Join(m.Work, name))
	return err
}

// fetchBinlogs downloads the archived logs from first on, in order.
func (m *MariaDB) fetchBinlogs(ctx context.Context, first string) ([]string, error) {
	entries, err := m.Store.List(ctx, m.Catalog.LogPrefix())
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		name := strings.TrimSuffix(path.Base(entry.Key), storage.Suffix)
		if name < first {
			continue
		}
		file := filepath.Join(m.Work, name)
		if err := m.Store.DownloadFile(ctx, entry.Key, file); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, nil
}

// closedBinlogs parses SHOW BINARY LOGS output and drops the active log.
func closedBinlogs(listing string) []string {
	var names []string
	for _, line := range strings.Split(listing, "\n") {
		name, _, _ := strings.Cut(line, "\t")
		names = append(names, name)
	}
	return names[:len(names)-1]
}
