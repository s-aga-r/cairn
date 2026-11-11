package catalog

import (
	"context"
	"fmt"
	"time"

	"github.com/s-aga-r/cairn/internal/config"
)

// Set is a list of backups of one database, oldest first.
type Set []Backup

func (s Set) Find(id string) (*Backup, error) {
	for i := range s {
		if s[i].ID == id {
			return &s[i], nil
		}
	}
	return nil, fmt.Errorf("backup %q not found; run `cairn list` to see available backups", id)
}

// Latest returns the newest backup that match accepts, or nil.
func (s Set) Latest(match func(*Backup) bool) *Backup {
	for i := len(s) - 1; i >= 0; i-- {
		if match(&s[i]) {
			return &s[i]
		}
	}
	return nil
}

func (s Set) LatestPhysical() *Backup {
	return s.Latest((*Backup).Physical)
}

// Chain returns the full backup that id builds on, followed by every
// incremental up to and including id.
func (s Set) Chain(id string) (Set, error) {
	var chain Set
	for id != "" {
		backup, err := s.Find(id)
		if err != nil {
			return nil, fmt.Errorf("broken backup chain: %w", err)
		}
		chain = append(Set{*backup}, chain...)
		id = backup.Parent
	}
	return chain, nil
}

// Plan lists what a prune removes.
type Plan struct {
	Backups Set
	Logs    []string
}

// Plan applies the retention policy. It keeps the newest fulls together
// with their incrementals, and the logs needed to replay from the oldest
// kept physical backup.
func (c *Catalog) Plan(ctx context.Context, all Set, retention config.Retention) (Plan, error) {
	var plan Plan
	expired := all.expiredFulls(retention.KeepFull)
	logical := 0
	var oldestKept *Backup
	for i := len(all) - 1; i >= 0; i-- {
		backup := &all[i]
		switch {
		case !backup.Physical():
			logical++
			if retention.KeepLogical > 0 && logical > retention.KeepLogical {
				plan.Backups = append(plan.Backups, *backup)
			}
		case expired[all.root(backup)]:
			plan.Backups = append(plan.Backups, *backup)
		default:
			oldestKept = backup
		}
	}
	if len(expired) == 0 || oldestKept == nil {
		return plan, nil
	}
	logs, err := c.logsBefore(ctx, oldestKept.StartedAt)
	plan.Logs = logs
	return plan, err
}

// Apply deletes what the plan lists. Manifests go first so a partial
// prune never leaves a listed backup without data.
func (c *Catalog) Apply(ctx context.Context, plan Plan) error {
	var manifests, data []string
	for _, backup := range plan.Backups {
		manifests = append(manifests, backup.prefix()+manifestName)
		for _, object := range backup.Objects {
			data = append(data, backup.Key(object.Name))
		}
	}
	if err := c.Store.Delete(ctx, manifests); err != nil {
		return err
	}
	return c.Store.Delete(ctx, append(data, plan.Logs...))
}

// expiredFulls returns the IDs of full backups beyond the newest keep.
func (s Set) expiredFulls(keep int) map[string]bool {
	expired := map[string]bool{}
	if keep <= 0 {
		return expired
	}
	seen := 0
	for i := len(s) - 1; i >= 0; i-- {
		if s[i].Type != config.TypeFull {
			continue
		}
		seen++
		if seen > keep {
			expired[s[i].ID] = true
		}
	}
	return expired
}

// root returns the ID of the full backup a physical backup builds on.
// A backup whose chain is broken is its own root, so it is kept.
func (s Set) root(backup *Backup) string {
	chain, err := s.Chain(backup.ID)
	if err != nil {
		return backup.ID
	}
	return chain[0].ID
}

// logMargin covers clock differences between this host and the storage
// server, whose timestamps also have only one-second resolution.
const logMargin = time.Hour

// logsBefore lists archived logs uploaded well before cutoff. A log
// needed by a backup is always archived after that backup starts.
func (c *Catalog) logsBefore(ctx context.Context, cutoff time.Time) ([]string, error) {
	entries, err := c.Store.List(ctx, c.LogPrefix())
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, entry := range entries {
		if entry.Modified.Before(cutoff.Add(-logMargin)) {
			keys = append(keys, entry.Key)
		}
	}
	return keys, nil
}
