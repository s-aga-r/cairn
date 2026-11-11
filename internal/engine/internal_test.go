package engine

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/s-aga-r/cairn/internal/catalog"
)

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func archive(t *testing.T, files map[string]string) *bytes.Buffer {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for name, content := range files {
		writer.WriteHeader(&tar.Header{Name: name, Size: int64(len(content)), Mode: 0o600})
		writer.Write([]byte(content))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &buffer
}

func TestCopyTarEntry(t *testing.T) {
	files := map[string]string{"base/1": "table data", "backup_manifest": "the manifest"}
	var manifest bytes.Buffer
	if err := copyTarEntry(archive(t, files), "backup_manifest", &manifest); err != nil || manifest.String() != "the manifest" {
		t.Fatalf("got %q, %v", manifest.String(), err)
	}
	if err := copyTarEntry(archive(t, files), "backup_manifest", brokenWriter{}); err == nil {
		t.Fatal("a failing writer must fail the copy")
	}
	garbage := strings.NewReader(strings.Repeat("not a tar archive", 100))
	if err := copyTarEntry(garbage, "backup_manifest", io.Discard); err == nil {
		t.Fatal("a corrupt stream must fail")
	}
}

func TestRecordPositionNeedsInfoFile(t *testing.T) {
	if err := recordPosition(&catalog.Backup{}, t.TempDir()); err == nil {
		t.Fatal("a missing info file must fail")
	}
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	buffer := &tailBuffer{limit: 8}
	for range 10 {
		buffer.Write([]byte("0123456789"))
	}
	if got := buffer.String(); got != "23456789" {
		t.Fatalf("got %q", got)
	}
}

func TestAppendFileNeedsDirectory(t *testing.T) {
	if err := appendFile(filepath.Join(t.TempDir(), "missing", "file"), "text"); err == nil {
		t.Fatal("a missing directory must fail")
	}
}
