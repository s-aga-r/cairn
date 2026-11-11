package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const valid = `{
  "storage": {"endpoint": "http://127.0.0.1:9000", "bucket": "b", "prefix": "/nested/prefix/", "secret_key": "${CAIRN_TEST_SECRET}"},
  "databases": {"main": {"engine": "postgres"}}
}`

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cairn.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAppliesDefaultsAndEnvironment(t *testing.T) {
	t.Setenv("CAIRN_TEST_SECRET", `quote"and\slash`)
	cfg, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	db := cfg.Databases["main"]
	if cfg.Storage.SecretKey != `quote"and\slash` || cfg.Storage.Prefix != "nested/prefix" ||
		cfg.Storage.PartSizeMB != 64 || cfg.Compression.Level != 3 || db.Name != "main" ||
		db.DefaultType != TypeAuto || db.FullIntervalHours != 168 || !filepath.IsAbs(cfg.Path) {
		t.Fatalf("unexpected config: %+v %+v", cfg, db)
	}
}

func TestLoadRejectsBadFiles(t *testing.T) {
	t.Setenv("CAIRN_TEST_SECRET", "x")
	cases := map[string]string{
		"environment variables not set: CAIRN_UNSET_VARIABLE": `{"storage": {"bucket": "${CAIRN_UNSET_VARIABLE}"}}`,
		"parse config":                            `{"storage": `,
		`unknown field "bukket"`:                  `{"storage": {"bukket": "b"}}`,
		"storage.endpoint is required":            `{}`,
		"storage.bucket is required":              `{}`,
		"at least one entry in databases":         `{}`,
		"part_size_mb must be at least 5":         `{"storage": {"part_size_mb": 1}}`,
		"compression.level must be between":       `{"compression": {"level": 99}}`,
		"databases.a b: name must not contain":    `{"databases": {"a b": {"engine": "mariadb"}}}`,
		"databases.x.engine must be":              `{"databases": {"x": {"engine": "oracle"}}}`,
		"databases.x.default_type must be one of": `{"databases": {"x": {"engine": "mariadb", "default_type": "weekly"}}}`,
	}
	for want, content := range cases {
		if _, err := Load(write(t, content)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("config %s: want error containing %q, got %v", content, want, err)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("a missing file must fail")
	}
}

func TestDatabaseSelection(t *testing.T) {
	single := &Config{Databases: map[string]*Database{"only": {}}}
	if db, err := single.Database(""); err != nil || db != single.Databases["only"] {
		t.Fatalf("the only database should be the default: %v", err)
	}
	several := &Config{Databases: map[string]*Database{"a": {}, "b": {}}}
	if _, err := several.Database(""); err == nil || !strings.Contains(err.Error(), "name one of: a, b") {
		t.Fatalf("want a hint with the names, got %v", err)
	}
	if _, err := several.Database("c"); err == nil || !strings.Contains(err.Error(), `unknown database "c"`) {
		t.Fatalf("want unknown database, got %v", err)
	}
	if db, err := several.Database("b"); err != nil || db != several.Databases["b"] {
		t.Fatalf("lookup by name failed: %v", err)
	}
}

func TestLocate(t *testing.T) {
	if path, _ := Locate("explicit.json"); path != "explicit.json" {
		t.Fatalf("explicit path must win, got %q", path)
	}
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CAIRN_CONFIG", "")
	if _, err := Locate(""); err == nil || !strings.Contains(err.Error(), "cairn init") {
		t.Fatalf("want a hint to run init, got %v", err)
	}
	fromEnv := write(t, valid)
	t.Setenv("CAIRN_CONFIG", fromEnv)
	if path, err := Locate(""); err != nil || path != fromEnv {
		t.Fatalf("CAIRN_CONFIG should be found, got %q %v", path, err)
	}
}
