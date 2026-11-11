package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// toolbox builds commands for the external database tools.
type toolbox struct {
	binDir string
	env    []string
}

func (t toolbox) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, t.path(name), args...)
	cmd.Env = append(os.Environ(), t.env...)
	return cmd
}

// path prefers the configured bin_dir so several server versions can
// coexist, and falls back to PATH for tools that live elsewhere.
func (t toolbox) path(name string) string {
	candidate := filepath.Join(t.binDir, name)
	if _, err := os.Stat(candidate); t.binDir != "" && err == nil {
		return candidate
	}
	return name
}

// missing reports which of the named tools cannot be found.
func (t toolbox) missing(names ...string) []string {
	var missing []string
	for _, name := range names {
		if _, err := exec.LookPath(t.path(name)); err != nil {
			missing = append(missing, name)
		}
	}
	return missing
}

// run executes the command and attaches the end of its stderr to a failure.
func run(cmd *exec.Cmd) error {
	stderr := &tailBuffer{limit: 2048}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w\n%s", filepath.Base(cmd.Path), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func output(cmd *exec.Cmd) (string, error) {
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := run(cmd)
	return strings.TrimSpace(stdout.String()), err
}

// pipe runs producer | consumer and waits for both.
func pipe(producer, consumer *exec.Cmd) error {
	reader, writer := io.Pipe()
	producer.Stdout, consumer.Stdin = writer, reader
	produced := make(chan error, 1)
	go func() {
		err := run(producer)
		writer.CloseWithError(err)
		produced <- err
	}()
	err := run(consumer)
	// Unblocks the producer when the consumer exited early.
	reader.CloseWithError(err)
	return errors.Join(<-produced, err)
}

// nonEmpty formats "key=value" pairs, skipping empty values.
func nonEmpty(pairs ...string) []string {
	var out []string
	for i := 0; i < len(pairs); i += 2 {
		if pairs[i+1] != "" && pairs[i+1] != "0" {
			out = append(out, pairs[i]+"="+pairs[i+1])
		}
	}
	return out
}

// tailBuffer keeps the last bytes written to it. Backup tools log
// progress to stderr, and only the end explains a failure.
type tailBuffer struct {
	limit int
	data  []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	if len(b.data) > 2*b.limit {
		b.data = append([]byte(nil), b.data[len(b.data)-b.limit:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	return string(b.data[max(0, len(b.data)-b.limit):])
}
