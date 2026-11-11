package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// PushWAL archives one WAL file. PostgreSQL calls it through archive_command.
func (p *Postgres) PushWAL(ctx context.Context, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return p.Store.PutCompressed(ctx, p.Catalog.LogKey(filepath.Base(path)), data)
}

// FetchWAL restores one WAL file. PostgreSQL calls it through
// restore_command and treats a failure as the end of the archive.
func (p *Postgres) FetchWAL(ctx context.Context, name, dest string) error {
	return p.Store.DownloadFile(ctx, p.Catalog.LogKey(name), dest)
}

// configureRecovery makes the restored cluster replay archived WAL on
// its first start, up to the requested time. Without a recovery request
// the cluster starts from the WAL inside the backup.
func (p *Postgres) configureRecovery(dataDir string, options RestoreOptions) error {
	if !options.Recover {
		return nil
	}
	// Executable fails only on exotic platforms; PATH lookup still works then.
	binary, _ := os.Executable()
	settings := fmt.Sprintf("\n# Added by cairn restore.\nrestore_command = '\"%s\" --config \"%s\" wal fetch %s %%f \"%%p\"'\n",
		binary, p.Config.Path, p.DB.Name)
	if !options.Until.IsZero() {
		settings += fmt.Sprintf("recovery_target_time = '%s'\nrecovery_target_action = 'promote'\n",
			options.Until.UTC().Format("2006-01-02 15:04:05.000000+00"))
	}
	return errors.Join(
		appendFile(filepath.Join(dataDir, "postgresql.auto.conf"), settings),
		os.WriteFile(filepath.Join(dataDir, "recovery.signal"), nil, 0o600),
	)
}

func appendFile(path, text string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = file.WriteString(text)
	return errors.Join(err, file.Close())
}
