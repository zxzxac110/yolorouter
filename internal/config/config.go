// Package config loads and manages the config file: built-in defaults, strict
// YAML parsing, and auto-generation of configs/config.yaml on first run.
package config

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Log      LogConfig      `yaml:"log"`
	Security SecurityConfig `yaml:"security"`
	Update   UpdateConfig   `yaml:"update"`
}

type ServerConfig struct {
	Port int `yaml:"port"`
}

type DatabaseConfig struct {
	Driver     string `yaml:"driver"`
	SQLitePath string `yaml:"sqlite_path"`
	Host       string `yaml:"host"`
	Port       int    `yaml:"port"`
	User       string `yaml:"user"`
	Password   string `yaml:"password"`
	DBName     string `yaml:"dbname"`
	// SSLMode is a libpq sslmode value (disable/require/verify-ca/verify-full).
	// Defaults to "disable" for local-dev convenience; remote/production
	// Postgres deployments should set this explicitly (e.g. "require" or
	// "verify-full") to avoid sending credentials and data in the clear.
	SSLMode string `yaml:"sslmode"`
}

type LogConfig struct {
	Level string `yaml:"level"`
}

type SecurityConfig struct {
	ProviderMasterKey string `yaml:"provider_master_key"`
	// AllowPrivateUpstreams, when true, lets the SSRF guard dial
	// loopback/private/link-local/CGNAT/unique-local destinations for BOTH
	// connection tests and gateway relay (multicast, benchmark, reserved, and
	// unspecified addresses stay blocked regardless). Off by default: only a
	// single-tenant, self-hosted operator who deliberately points Yolorouter
	// at a LAN/localhost model server (Ollama, vLLM, LM Studio, one-api, …)
	// should turn it on. Never enable it on a multi-tenant or internet-exposed
	// deployment — it lets a provider base_url reach internal services and
	// cloud metadata endpoints (169.254.169.254).
	AllowPrivateUpstreams bool `yaml:"allow_private_upstreams"`
}

// UpdateConfig controls the version-update feature (the background update
// check surfaced via the system info API + the `update` CLI). Enabled
// defaults true so an auto-generated or legacy config that omits the whole
// `update` section does not silently disable updates — only an explicit
// `enabled: false` does. GitHubRepo overrides the binary's compiled-in
// version.DefaultGitHubRepo; empty falls back to it, and both empty (or
// Enabled=false) disable the feature entirely.
//
// GitHubProxy, when non-empty, is a prefix through which every update-related
// GitHub request (release lookup + asset download) is routed, for deployments
// where GitHub is slow or blocked. Empty means direct GitHub. On a mirror
// install it is seeded automatically from the YOLO_UPDATE_GITHUB_PROXY
// environment variable when the config is first generated.
type UpdateConfig struct {
	Enabled     bool   `yaml:"enabled"`
	GitHubRepo  string `yaml:"github_repo"`
	GitHubProxy string `yaml:"github_proxy"`
}

func defaults() *Config {
	return &Config{
		Server: ServerConfig{Port: 8080},
		// Relative paths resolve against the config file's own directory
		// (see loadStrict below), and the default config lives at
		// configs/config.yaml — so "../data" lands the default data
		// directory as a top-level sibling of configs/, not nested inside
		// it.
		Database: DatabaseConfig{Driver: "sqlite", SQLitePath: "../data/yolorouter.db", SSLMode: "disable"},
		Log:      LogConfig{Level: "info"},
		// Enabled defaults true so a config that omits the `update` section
		// entirely (auto-generated, legacy) keeps updates ON — only an
		// explicit `enabled: false` disables them.
		Update: UpdateConfig{Enabled: true},
	}
}

// Load resolves the config path (explicitPath wins if non-empty, otherwise
// delegates to resolveConfigPath), then:
//   - if the path exists: strict-parse it, no auto-generation ever happens
//   - if explicitPath was given but doesn't exist: hard error
//   - if using a default path and it doesn't exist: apply built-in
//     defaults, generate a random provider_master_key, and atomically write
//     the effective config out to that path so restarts reuse the same key
func resolveConfigPath() string {
	// 1. Environment variable takes highest priority.
	if p := os.Getenv("YOLOROUTER_CONFIG"); p != "" {
		return p
	}

	// 2. Derive from the executable's location.
	//    e.g. /path/to/bin/yolorouter.exe -> /path/to/configs/config.yaml
	if exe, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), "..", "configs", "config.yaml"); p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}

	// 3. Fall back to the current working directory (legacy behaviour).
	return filepath.Join("configs", "config.yaml")
}

func Load(explicitPath string) (*Config, error) {
	path := explicitPath
	usingDefaultPath := false
	if path == "" {
		path = resolveConfigPath()
		usingDefaultPath = true
	}

	if _, err := os.Stat(path); err != nil {
		if !usingDefaultPath {
			return nil, fmt.Errorf("config file not found at explicit path %s: %w", path, err)
		}
		return generateDefaultConfig(path)
	}

	cfg, err := loadStrict(path)
	if err != nil {
		return nil, err
	}
	// An existing config is never regenerated, so a mirror installer upgrading a
	// prior direct install can't seed the proxy at generation time. Fill it from
	// the env the installer injects into the service unit — only when the file
	// leaves it empty, so an explicit value in the config still wins.
	applyUpdateProxyEnv(cfg)
	return cfg, nil
}

// applyUpdateProxyEnv fills update.github_proxy from YOLO_UPDATE_GITHUB_PROXY
// when the config leaves it empty, so the env var the mirror installer injects
// into the service unit takes effect without overriding an explicit config value.
func applyUpdateProxyEnv(cfg *Config) {
	if cfg.Update.GitHubProxy == "" {
		if proxy := os.Getenv("YOLO_UPDATE_GITHUB_PROXY"); proxy != "" {
			cfg.Update.GitHubProxy = proxy
		}
	}
}

func generateDefaultConfig(path string) (*Config, error) {
	cfg := defaults()
	key, err := randomMasterKey()
	if err != nil {
		return nil, fmt.Errorf("generate provider_master_key: %w", err)
	}
	cfg.Security.ProviderMasterKey = key

	// A mirror install (install.sh with YOLO_MIRROR) exports this, so record the
	// proxy in the generated file — self-update then routes through the same
	// mirror with no manual edit. Persisted here (before the write) so the CLI
	// `update`, run from a shell without the env, also reads it from config.
	applyUpdateProxyEnv(cfg)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create config directory: %w", err)
	}

	// The SQLite path is resolved relative to the config file's directory
	// (see loadStrict below), not the process cwd, so the data directory
	// must be created at that same resolved location.
	absConfigDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("resolve config directory: %w", err)
	}
	sqlitePath := cfg.Database.SQLitePath
	if !filepath.IsAbs(sqlitePath) {
		sqlitePath = filepath.Join(absConfigDir, sqlitePath)
	}
	if err := os.MkdirAll(filepath.Dir(sqlitePath), 0o755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	if err := atomicWriteConfig(path, cfg); err != nil {
		return nil, err
	}

	// Re-read to handle the race where a concurrent process won the write.
	return loadStrict(path)
}

func atomicWriteConfig(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal generated config: %w", err)
	}

	// Use a unique temp filename (not a fixed path+".tmp") so two processes
	// racing to auto-generate the same config on first boot can't clobber or
	// truncate each other's in-progress temp file.
	tmpFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp config file: %w", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp config file: %w", err)
	}
	if err := tmpFile.Chmod(0o600); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod temp config file: %w", err)
	}
	// fsync before Close so the freshly-generated provider_master_key is
	// actually durable on disk — without this, a crash or power loss right
	// after Link "publishes" the file below could leave an entry pointing
	// at content the kernel never flushed, and the only copy of that key
	// (nothing else knows it) is gone.
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sync temp config file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp config file: %w", err)
	}

	// Publish via a hard link, not Stat-then-Rename: Stat-then-Rename has a
	// TOCTOU race — two processes can both observe "doesn't exist" and both
	// proceed, and a plain os.Rename never fails just because the
	// destination already exists, so the second process to rename silently
	// overwrites the first process's file (and, worse, its
	// already-generated master key — anything encrypted under the first
	// key becomes unreadable). os.Link atomically fails with an "already
	// exists" error if the destination is taken, so at most one of any
	// racing processes ever successfully publishes.
	if err := os.Link(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		if errors.Is(err, os.ErrExist) {
			// Lost the race to another process generating the same file
			// first; discard our version and let the caller re-read the
			// winner.
			return nil
		}
		return fmt.Errorf("finalize config file: %w", err)
	}
	_ = os.Remove(tmpPath) // tmpPath and path are now two links to the same inode

	// Best-effort: fsync the parent directory too, so the new directory
	// entry itself (not just the file's data) survives a crash. Not fatal
	// if unsupported (some filesystems reject fsync on a directory fd) —
	// the file's own content is already durable from the Sync above either way.
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func loadStrict(path string) (*Config, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat config file %s: %w", path, err)
	}
	// The file holds security.provider_master_key in plaintext — treat it
	// like any other secret file and reject group/other-readable permission
	// bits (this only carries meaning on Unix; Go emulates harmless
	// permission bits on Windows, so the check is a no-op there).
	if runtime.GOOS != "windows" {
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("config file %s must not be group- or other-readable (mode %04o); run chmod 600 %s", path, info.Mode().Perm(), path)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %s: %w", path, err)
	}

	cfg := defaults()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config file %s: %w", path, err)
	}
	// yaml.Decoder.Decode only consumes the first "---"-delimited document
	// in the stream — decoding again and requiring io.EOF here rejects a
	// config.yaml with a second document instead of silently ignoring it,
	// which could otherwise hide a real config value the file's author
	// expected to take effect.
	if err := decoder.Decode(new(Config)); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("config file %s contains more than one YAML document", path)
		}
		return nil, fmt.Errorf("parse config file %s: %w", path, err)
	}

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("invalid config file %s: %w", path, err)
	}

	absDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("resolve config directory: %w", err)
	}
	if !filepath.IsAbs(cfg.Database.SQLitePath) {
		cfg.Database.SQLitePath = filepath.Join(absDir, cfg.Database.SQLitePath)
	}

	return cfg, nil
}

// validSSLModes is libpq's known sslmode value set — validated only when
// database.driver is "postgres" (SQLite deployments carry the SSLMode field's
// harmless "disable" zero-value default and never use it).
var validSSLModes = map[string]bool{
	"disable": true, "allow": true, "prefer": true,
	"require": true, "verify-ca": true, "verify-full": true,
}

func validate(cfg *Config) error {
	if cfg.Database.Driver != "sqlite" && cfg.Database.Driver != "postgres" {
		return fmt.Errorf("database.driver must be \"sqlite\" or \"postgres\", got %q", cfg.Database.Driver)
	}
	if cfg.Database.Driver == "postgres" {
		if cfg.Database.Host == "" {
			return fmt.Errorf("database.host must not be empty when database.driver is \"postgres\"")
		}
		if cfg.Database.User == "" {
			return fmt.Errorf("database.user must not be empty when database.driver is \"postgres\"")
		}
		if cfg.Database.DBName == "" {
			return fmt.Errorf("database.dbname must not be empty when database.driver is \"postgres\"")
		}
		if cfg.Database.Port <= 0 || cfg.Database.Port > 65535 {
			return fmt.Errorf("database.port must be between 1 and 65535 when database.driver is \"postgres\", got %d", cfg.Database.Port)
		}
		if !validSSLModes[cfg.Database.SSLMode] {
			return fmt.Errorf("database.sslmode must be one of disable/allow/prefer/require/verify-ca/verify-full, got %q", cfg.Database.SSLMode)
		}
	}
	if cfg.Server.Port <= 0 || cfg.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535, got %d", cfg.Server.Port)
	}
	if !validLogLevels[cfg.Log.Level] {
		return fmt.Errorf("log.level must be one of debug/info/warn/error, got %q", cfg.Log.Level)
	}
	if err := validateMasterKey(cfg.Security.ProviderMasterKey); err != nil {
		return fmt.Errorf("security.provider_master_key: %w", err)
	}
	if err := validateGitHubRepo(cfg.Update.GitHubRepo); err != nil {
		return fmt.Errorf("update.github_repo: %w", err)
	}
	return nil
}

// validateGitHubRepo accepts an empty repo (falls back to the compiled-in
// version.DefaultGitHubRepo, or disables updates if that is also empty) but
// rejects a malformed non-empty value early — a typo like "ownerrepo" or
// "owner/repo/extra" would otherwise only surface as a 404 from GitHub's
// releases API at runtime, with no hint that the config value was the cause.
func validateGitHubRepo(repo string) error {
	if repo == "" {
		return nil
	}
	if strings.ContainsAny(repo, " \t") {
		return fmt.Errorf("must not contain whitespace, got %q", repo)
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("must be %q with exactly one slash, got %q", "owner/repo", repo)
	}
	return nil
}

// validLogLevels are the level strings pkg/logger.Init actually recognizes
// via zapcore.Level.UnmarshalText. That function's own copied-verbatim
// implementation silently falls back to info on any unparseable value
// rather than erroring — validating here instead means a typo'd log.level
// (e.g. "debu") fails loudly at config-load time instead of silently
// running at the wrong verbosity forever.
var validLogLevels = map[string]bool{
	"debug": true, "info": true, "warn": true, "error": true,
}

// validateMasterKey requires a standard-base64-encoded 32-byte AES-256 key,
// matching what randomMasterKey generates — not just "non-empty", since a
// malformed or wrong-length key would only surface as a confusing failure
// later, at first encrypt/decrypt use.
func validateMasterKey(key string) error {
	if key == "" {
		return fmt.Errorf("must not be empty")
	}
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return fmt.Errorf("must be standard base64, got invalid value: %w", err)
	}
	if len(decoded) != 32 {
		return fmt.Errorf("must decode to exactly 32 bytes (AES-256), got %d", len(decoded))
	}
	return nil
}

func randomMasterKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}
