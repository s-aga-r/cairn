package cli

import (
	"fmt"
	"time"

	"github.com/s-aga-r/cairn/internal/engine"
)

// engineDefaults selects backups without recovery constraints.
var engineDefaults = engine.RestoreOptions{}

func formatBytes(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	value, unit := float64(n), 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", value, units[unit])
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

func orDash(text string) string {
	if text == "" {
		return "-"
	}
	return text
}
