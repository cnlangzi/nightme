// Package config loads nightme configuration from a YAML file and
// applies environment-variable overrides.
//
// Resolution order (later wins):
//
//  1. Hard-coded defaults applied to every Config.
//  2. Values deserialized from the YAML file (if Load finds it).
//  3. Values from NIGHTME_<SECTION>_<KEY> environment variables.
//
// Environment variables are useful for secrets (Feishu AppSecret) and
// CI overrides; the YAML file is the source of truth for humans.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the root of the on-disk configuration. Field tags drive
// YAML deserialization; the struct itself is the public API.
type Config struct {
	Feishu  FeishuConfig  `yaml:"feishu"`
	Agents  AgentsConfig  `yaml:"agents"`
	Session SessionConfig `yaml:"session"`
	Logging LoggingConfig `yaml:"logging"`
	Paths   PathsConfig   `yaml:"paths"`
}

// FeishuConfig holds credentials for the Feishu (Lark) channel.
// AppSecret is the only sensitive field; the rest are public IDs.
type FeishuConfig struct {
	AppID             string `yaml:"app_id"`
	AppSecret         string `yaml:"app_secret"`
	VerificationToken string `yaml:"verification_token"`
	EncryptKey        string `yaml:"encrypt_key"`
}

// AgentsConfig declares the global default agent (v1.2 Q-A) and
// the per-agent spawn recipes used by the Bridge.
//
// YAML shape (v1.2, post-rename):
//
//	agents:
//	  default: claude                 # global default (any registered agent name)
//	  claude:                         # inline recipe (sibling of default)
//	    command: claude
//	    args: []
//	    env: {}
//	  codex:
//	    command: codex-acp
//	    ...
//
// The inline map (yaml:",inline") captures every top-level key under
// `agents` except `default` — those become AgentEntry entries.
//
// `default` must be one of the registered agent names (validation
// happens at registry population time, not here).
type AgentsConfig struct {
	// Default is the global default agent name. v1.2 (Q-A): the
	// only user-facing Default; ChatSession.defaultAgent is a
	// snapshot of this value at ChatSession creation time.
	Default string `yaml:"default"`

	// Recipes maps agent name -> spawn recipe. Names must match
	// agent.Agent.Name() values; names not in the registry trigger
	// errors at session-create time.
	//
	// Inline: every sibling key under `agents:` other than
	// `default` lands here. The previous v1.x schema had this under
	// a nested `agents:` key; v1.2 flattens it.
	Recipes map[string]AgentEntry `yaml:",inline"`
}

// AgentEntry is the spawn recipe for one CLI.
type AgentEntry struct {
	// Command is the executable name (resolved via PATH) or an
	// absolute path.
	Command string `yaml:"command"`

	// Args is appended after the agent's own defaults. v0.1 typically
	// empty.
	Args []string `yaml:"args"`

	// Env is merged into the child process environment. v0.1
	// typically empty.
	Env map[string]string `yaml:"env"`
}

// SessionConfig holds runtime tunables for the Session Manager.
type SessionConfig struct {
	// DefaultPtyCols is the initial PTY width (characters) for a new
	// Claude Code / Codex / OpenCode session.
	DefaultPtyCols int `yaml:"default_pty_cols"`

	// DefaultPtyRows is the initial PTY height (lines).
	DefaultPtyRows int `yaml:"default_pty_rows"`

	// OutputChunkSize is the byte threshold the Aggregator uses before
	// flushing to the channel (4 KiB is a sensible default).
	OutputChunkSize int `yaml:"output_chunk_size"`

	// OutputFlushIntervalMs is the wall-clock flush window in
	// milliseconds.
	OutputFlushIntervalMs int `yaml:"output_flush_interval_ms"`
}

// LoggingConfig drives log/slog.
type LoggingConfig struct {
	// Level is one of debug, info, warn, error (case-insensitive).
	Level string `yaml:"level"`

	// File is the optional log file path. Empty means stdout.
	File string `yaml:"file"`
}

// PathsConfig holds filesystem locations for runtime state.
type PathsConfig struct {
	// DataDir is the root for registry.json, sessions.json, and
	// per-session scratch.
	DataDir string `yaml:"data_dir"`

	// RegistryFile is the JSON file backing the process registry.
	RegistryFile string `yaml:"registry_file"`

	// SessionsFile is the JSON file backing session persistence.
	SessionsFile string `yaml:"sessions_file"`
}

// DefaultPath is the conventional location of the nightme config
// file on Linux/macOS (XDG-style with a fallback).
func DefaultPath() string {
	// Respect $NIGHTME_CONFIG if set.
	if v := os.Getenv("NIGHTME_CONFIG"); v != "" {
		return v
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "nightme", "config.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "nightme", "config.yaml")
}

// LoadDefault loads Config from DefaultPath(). If the file is missing
// it returns a Config populated entirely from defaults (no error).
func LoadDefault() (*Config, error) {
	return Load(DefaultPath())
}

// SaveDefault writes cfg atomically to DefaultPath(). Directories
// are created as needed (0700); the file is chmod 0600 (N-7).
func SaveDefault(c *Config) error {
	return Save(c, DefaultPath())
}

// Save writes cfg atomically to path. Same temp-file+rename pattern
// as the registry: a corrupted temp cannot leave a half-written
// config.yaml on disk.
func Save(c *Config, path string) error {
	if path == "" {
		return fmt.Errorf("config: empty path")
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: mkdir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("config: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("config: rename: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("config: chmod: %w", err)
	}
	return nil
}

// Load reads a YAML file from path and returns a populated Config.
// Missing file is not an error — defaults are returned. A malformed
// file is an error.
func Load(path string) (*Config, error) {
	c := &Config{}

	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(data, c); err != nil {
				return nil, fmt.Errorf("config: parse %s: %w", path, err)
			}
		case errors.Is(err, os.ErrNotExist):
			// missing file is fine; we'll fill defaults below
		default:
			return nil, fmt.Errorf("config: read %s: %w", path, err)
		}
	}

	applyDefaults(c)
	applyEnvOverrides(c)
	expandHomePaths(c)
	return c, nil
}

// applyDefaults populates zero-valued fields with the shipped
// defaults. It does not overwrite non-zero values.
func applyDefaults(c *Config) {
	if c.Agents.Default == "" {
		c.Agents.Default = "claude"
	}
	if c.Session.DefaultPtyCols == 0 {
		c.Session.DefaultPtyCols = 80
	}
	if c.Session.DefaultPtyRows == 0 {
		c.Session.DefaultPtyRows = 24
	}
	if c.Session.OutputChunkSize == 0 {
		c.Session.OutputChunkSize = 4096
	}
	if c.Session.OutputFlushIntervalMs == 0 {
		c.Session.OutputFlushIntervalMs = 200
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Paths.DataDir == "" {
		c.Paths.DataDir = "~/.local/share/nightme"
	}
	if c.Paths.RegistryFile == "" {
		c.Paths.RegistryFile = "registry.json"
	}
	if c.Paths.SessionsFile == "" {
		c.Paths.SessionsFile = "sessions.json"
	}
}

// applyEnvOverrides looks at every NIGHTME_<SECTION>_<KEY> variable
// and writes the value into the matching struct field. Unknown
// variables are silently ignored (forward-compatibility).
func applyEnvOverrides(c *Config) {
	if v := os.Getenv("NIGHTME_FEISHU_APP_ID"); v != "" {
		c.Feishu.AppID = v
	}
	if v := os.Getenv("NIGHTME_FEISHU_APP_SECRET"); v != "" {
		c.Feishu.AppSecret = v
	}
	if v := os.Getenv("NIGHTME_FEISHU_VERIFICATION_TOKEN"); v != "" {
		c.Feishu.VerificationToken = v
	}
	if v := os.Getenv("NIGHTME_FEISHU_ENCRYPT_KEY"); v != "" {
		c.Feishu.EncryptKey = v
	}
	if v := os.Getenv("NIGHTME_AGENT_DEFAULT"); v != "" {
		c.Agents.Default = v
	}
	if v := os.Getenv("NIGHTME_LOGGING_LEVEL"); v != "" {
		c.Logging.Level = v
	}
	if v := os.Getenv("NIGHTME_LOGGING_FILE"); v != "" {
		c.Logging.File = v
	}
	if v := os.Getenv("NIGHTME_PATHS_DATA_DIR"); v != "" {
		c.Paths.DataDir = v
	}
	if v := os.Getenv("NIGHTME_PATHS_REGISTRY_FILE"); v != "" {
		c.Paths.RegistryFile = v
	}
	if v := os.Getenv("NIGHTME_PATHS_SESSIONS_FILE"); v != "" {
		c.Paths.SessionsFile = v
	}

	// Numeric session overrides — only set when the variable parses.
	if v := os.Getenv("NIGHTME_SESSION_DEFAULT_PTY_COLS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Session.DefaultPtyCols = n
		}
	}
	if v := os.Getenv("NIGHTME_SESSION_DEFAULT_PTY_ROWS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Session.DefaultPtyRows = n
		}
	}
	if v := os.Getenv("NIGHTME_SESSION_OUTPUT_CHUNK_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Session.OutputChunkSize = n
		}
	}
	if v := os.Getenv("NIGHTME_SESSION_OUTPUT_FLUSH_INTERVAL_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Session.OutputFlushIntervalMs = n
		}
	}
}

// expandHomePaths rewrites "~" / "~/..." in path fields to the user's
// home directory. Other path fields are left untouched.
func expandHomePaths(c *Config) {
	c.Paths.DataDir = expandHome(c.Paths.DataDir)
	c.Paths.RegistryFile = expandHome(c.Paths.RegistryFile)
	c.Paths.SessionsFile = expandHome(c.Paths.SessionsFile)
	c.Logging.File = expandHome(c.Logging.File)
}

func expandHome(p string) string {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}
