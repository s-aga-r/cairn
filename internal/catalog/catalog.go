// Package catalog keeps the record of backups stored in the bucket.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/s-aga-r/cairn/internal/config"
	"github.com/s-aga-r/cairn/internal/storage"
)

const manifestName = "manifest.json"

// Backup is the manifest of one backup. It is written last, so a backup
// without a manifest is incomplete and never listed.
type Backup struct {
	ID            string           `json:"id"`
	Database      string           `json:"database"`
	Engine        string           `json:"engine"`
	Type          string           `json:"type"`
	Parent        string           `json:"parent,omitempty"`
	ServerVersion string           `json:"server_version"`
	ToolVersion   string           `json:"tool_version"`
	StartedAt     time.Time        `json:"started_at"`
	FinishedAt    time.Time        `json:"finished_at"`
	Objects       []storage.Object `json:"objects"`
	// MariaDB only: where the next incremental and binlog replay start.
	LSN        string `json:"lsn,omitempty"`
	BinlogFile string `json:"binlog_file,omitempty"`
	BinlogPos  string `json:"binlog_pos,omitempty"`
}

func (b *Backup) Physical() bool {
	return b.Type != config.TypeLogical
}

// Key returns the storage key of a named object of this backup.
func (b *Backup) Key(name string) string {
	return b.prefix() + name + storage.Suffix
}

func (b *Backup) Size() (stored, raw int64) {
	for _, object := range b.Objects {
		stored += object.Size
		raw += object.RawSize
	}
	return stored, raw
}

func (b *Backup) prefix() string {
	return b.Database + "/backups/" + b.ID + "/"
}

type Catalog struct {
	Store    *storage.Store
	Database string
}

// New starts a manifest. The ID sorts by start time.
func (c *Catalog) New(engine, kind string, parent *Backup, toolVersion string) *Backup {
	now := time.Now().UTC()
	backup := &Backup{
		ID:          strings.Replace(now.Format("20060102-150405.000"), ".", "-", 1),
		Database:    c.Database,
		Engine:      engine,
		Type:        kind,
		ToolVersion: toolVersion,
		StartedAt:   now,
	}
	if parent != nil {
		backup.Parent = parent.ID
	}
	return backup
}

// Save commits the backup by writing its manifest.
func (c *Catalog) Save(ctx context.Context, backup *Backup) error {
	backup.FinishedAt = time.Now().UTC()
	// Marshal cannot fail for this struct.
	data, _ := json.MarshalIndent(backup, "", "  ")
	return c.Store.PutBytes(ctx, backup.prefix()+manifestName, data)
}

// Discard removes everything a failed backup left behind.
func (c *Catalog) Discard(ctx context.Context, backup *Backup) error {
	entries, err := c.Store.List(ctx, backup.prefix())
	if err != nil {
		return err
	}
	return c.Store.Delete(ctx, keysOf(entries))
}

// List returns all complete backups, oldest first.
func (c *Catalog) List(ctx context.Context) (Set, error) {
	entries, err := c.Store.List(ctx, c.Database+"/backups/")
	if err != nil {
		return nil, err
	}
	var backups Set
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Key, "/"+manifestName) {
			continue
		}
		backup, err := c.read(ctx, entry.Key)
		if err != nil {
			return nil, err
		}
		backups = append(backups, backup)
	}
	return backups, nil
}

// LogPrefix is where archived WAL segments and binary logs live.
func (c *Catalog) LogPrefix() string {
	return c.Database + "/logs/"
}

func (c *Catalog) LogKey(name string) string {
	return c.LogPrefix() + name + storage.Suffix
}

func (c *Catalog) read(ctx context.Context, key string) (Backup, error) {
	var backup Backup
	data, err := c.Store.GetBytes(ctx, key)
	if err == nil {
		err = json.Unmarshal(data, &backup)
	}
	if err != nil {
		return backup, fmt.Errorf("unreadable manifest %s: %w", key, err)
	}
	return backup, nil
}

func keysOf(entries []storage.Entry) []string {
	keys := make([]string, len(entries))
	for i, entry := range entries {
		keys[i] = entry.Key
	}
	return keys
}
