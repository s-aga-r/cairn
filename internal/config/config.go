// Package config loads and validates the JSON configuration file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
)

const (
	EngineMariaDB  = "mariadb"
	EnginePostgres = "postgres"
)

// Backup types. Auto is a policy, not a stored type.
const (
	TypeAuto        = "auto"
	TypeFull        = "full"
	TypeIncremental = "incremental"
	TypeLogical     = "logical"
)

var Types = []string{TypeAuto, TypeFull, TypeIncremental, TypeLogical}

type Config struct {
	Path        string               `json:"-"`
	Storage     Storage              `json:"storage"`
	Compression Compression          `json:"compression"`
	Databases   map[string]*Database `json:"databases"`
}

type Storage struct {
	Endpoint    string `json:"endpoint"`
	Region      string `json:"region"`
	Bucket      string `json:"bucket"`
	Prefix      string `json:"prefix"`
	AccessKey   string `json:"access_key"`
	SecretKey   string `json:"secret_key"`
	PartSizeMB  int    `json:"part_size_mb"`
	Concurrency int    `json:"concurrency"`
}

type Compression struct {
	Level   int `json:"level"`
	Threads int `json:"threads"`
}

type Database struct {
	Name              string    `json:"-"`
	Engine            string    `json:"engine"`
	Host              string    `json:"host"`
	Port              int       `json:"port"`
	Socket            string    `json:"socket"`
	User              string    `json:"user"`
	Password          string    `json:"password"`
	Databases         []string  `json:"databases"`
	BinDir            string    `json:"bin_dir"`
	DefaultsFile      string    `json:"defaults_file"`
	DefaultType       string    `json:"default_type"`
	FullIntervalHours int       `json:"full_interval_hours"`
	Parallel          int       `json:"parallel"`
	Retention         Retention `json:"retention"`
}

// Retention counts are "keep the newest N". Zero keeps everything.
type Retention struct {
	KeepFull    int `json:"keep_full"`
	KeepLogical int `json:"keep_logical"`
}

// Locate returns the config path to use: the explicit one, $CAIRN_CONFIG,
// or the first default location that exists.
func Locate(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	candidates := searchPaths()
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no config file found; pass --config, set CAIRN_CONFIG, or create one of: %s (try `cairn init`)",
		strings.Join(candidates, ", "))
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	expanded, err := expandEnv(string(raw))
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	cfg := &Config{}
	decoder := json.NewDecoder(strings.NewReader(expanded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	// Abs fails only when the working directory is gone; the relative path still works then.
	cfg.Path, _ = filepath.Abs(path)
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s:\n%w", path, err)
	}
	return cfg, nil
}

// Database returns the named database. An empty name selects the only
// configured database, so single-database setups need no argument.
func (c *Config) Database(name string) (*Database, error) {
	if name == "" && len(c.Databases) == 1 {
		name = c.Names()[0]
	}
	if db, ok := c.Databases[name]; ok {
		return db, nil
	}
	if name == "" {
		return nil, fmt.Errorf("several databases are configured; name one of: %s", strings.Join(c.Names(), ", "))
	}
	return nil, fmt.Errorf("unknown database %q; configured: %s", name, strings.Join(c.Names(), ", "))
}

func (c *Config) Names() []string {
	names := make([]string, 0, len(c.Databases))
	for name := range c.Databases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (c *Config) applyDefaults() {
	setDefault(&c.Storage.PartSizeMB, 64)
	setDefault(&c.Storage.Concurrency, 4)
	setDefault(&c.Compression.Level, 3)
	setDefault(&c.Compression.Threads, runtime.NumCPU())
	c.Storage.Prefix = strings.Trim(c.Storage.Prefix, "/")
	for name, db := range c.Databases {
		db.Name = name
		setDefault(&db.FullIntervalHours, 168)
		setDefault(&db.Parallel, min(4, runtime.NumCPU()))
		if db.DefaultType == "" {
			db.DefaultType = TypeAuto
		}
	}
}

func (c *Config) validate() error {
	var problems []error
	report := func(bad bool, format string, args ...any) {
		if bad {
			problems = append(problems, fmt.Errorf("  - "+format, args...))
		}
	}
	report(c.Storage.Endpoint == "", "storage.endpoint is required")
	report(c.Storage.Bucket == "", "storage.bucket is required")
	report(c.Storage.PartSizeMB < 5, "storage.part_size_mb must be at least 5")
	report(c.Compression.Level < 1 || c.Compression.Level > 22, "compression.level must be between 1 and 22")
	report(len(c.Databases) == 0, "at least one entry in databases is required")
	for _, name := range c.Names() {
		db := c.Databases[name]
		report(strings.ContainsAny(name, "/ "), "databases.%s: name must not contain '/' or spaces", name)
		report(db.Engine != EngineMariaDB && db.Engine != EnginePostgres,
			"databases.%s.engine must be %q or %q", name, EngineMariaDB, EnginePostgres)
		report(!slices.Contains(Types, db.DefaultType),
			"databases.%s.default_type must be one of %s", name, strings.Join(Types, ", "))
	}
	return errors.Join(problems...)
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${VAR} references so secrets can stay out of the file.
func expandEnv(text string) (string, error) {
	var missing []string
	expanded := envRef.ReplaceAllStringFunc(text, func(ref string) string {
		name := envRef.FindStringSubmatch(ref)[1]
		value, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
		}
		// The value lands inside a JSON string, so it must be escaped.
		quoted, _ := json.Marshal(value)
		return string(quoted[1 : len(quoted)-1])
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("environment variables not set: %s", strings.Join(missing, ", "))
	}
	return expanded, nil
}

func searchPaths() []string {
	var paths []string
	if env := os.Getenv("CAIRN_CONFIG"); env != "" {
		paths = append(paths, env)
	}
	paths = append(paths, "cairn.json")
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "cairn", "config.json"))
	}
	return append(paths, "/etc/cairn/config.json")
}

func setDefault(field *int, value int) {
	if *field == 0 {
		*field = value
	}
}
