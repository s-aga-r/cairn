package cli

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
)

// TestMain lets the test binary stand in for cairn itself: a restored
// PostgreSQL runs restore_command with the path of this executable.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "--config" {
		os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
	}
	// Failure tests point at dead endpoints; retries only slow them down.
	minio.MaxRetry = 1
	os.Exit(m.Run())
}

type result struct {
	stdout, stderr string
	code           int
}

// cairn runs the command line in process against a config file.
func cairn(config string, args ...string) result {
	var stdout, stderr bytes.Buffer
	code := Run(append([]string{"--config", config}, args...), &stdout, &stderr)
	return result{stdout.String(), stderr.String(), code}
}

// ok runs a command that must succeed and returns its stdout.
func ok(t *testing.T, config string, args ...string) string {
	t.Helper()
	r := cairn(config, args...)
	if r.code != 0 {
		t.Fatalf("cairn %v failed:\n%s%s", args, r.stdout, r.stderr)
	}
	return r.stdout
}

// fails runs a command that must fail with a message containing want.
func fails(t *testing.T, want, config string, args ...string) {
	t.Helper()
	r := cairn(config, args...)
	if r.code == 0 || !strings.Contains(r.stderr+r.stdout, want) {
		t.Fatalf("cairn %v: want failure containing %q, got code %d:\n%s%s", args, want, r.code, r.stdout, r.stderr)
	}
}

var backupID = regexp.MustCompile(`Backup (\S+) complete: (\w+)`)

// backup takes a backup and returns its ID and resolved type.
func backup(t *testing.T, config string, args ...string) (id, kind string) {
	t.Helper()
	match := backupID.FindStringSubmatch(ok(t, config, append([]string{"backup"}, args...)...))
	if match == nil {
		t.Fatal("backup printed no ID")
	}
	return match[1], match[2]
}

func want(t *testing.T, got, expected string) {
	t.Helper()
	if got != expected {
		t.Fatalf("got %q, want %q", got, expected)
	}
}
