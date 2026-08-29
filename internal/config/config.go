// Package config loads nightme configuration from a YAML file and
// applies environment-variable overrides.
//
// Resolution order (later wins):
//
//  1. Hard-coded defaults applied to every Config (applyDefaults).
//  2. Values deserialized from the YAML file (if Load finds it).
//  3. Values from NIGHTME_<SECTION>_<KEY> environment variables.
//  4. (LoadDefault only) Auto-detect cfg.Primary by probing the
//     registered builtins in registration order. The first one
//     whose Detect() succeeds becomes Primary and is persisted
//     to disk so subsequent starts don't re-probe. See
//     docs/primary-agent-detection.md for the full rationale.
//
// Environment variables are useful for secrets (Feishu AppSecret)
// and CI overrides; the YAML file is the source of truth for
// humans.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Config is the root of the on-disk configuration. Field tags drive
// YAML deserialization; the struct itself is the public API.
//
// v1.2 schema (post interactive-config refactor):
//
//	name: macbook-pro                # instance identifier (optional; falls back to os.Hostname())
//	primary: cc                      # global default agent (top-level)
//	agents:                          # list of available agents (top-level)
//	  - name: cc
//	    bridge: claude
//	    command: "claude --dangerously-skip-permissions"
//	  - name: codex
//	    bridge: codex
//	    command: codex               # spawns `codex app-server` via JSON-RPC 2.0
//	feishu: ...
//	session: ...
//	logging: ...
//	paths: ...
//
// User-configured `agents:` entries override built-in agents with the
// same name (merge happens at runtime, not parse time).
type Config struct {
	// Name identifies this nightme instance — i.e. which machine is
	// running the daemon. Surfaced later in logs / IM message headers
	// / gateway registrations so multiple machines can be told apart.
	//
	// If empty at runtime, callers should fall back to os.Hostname()
	// via EffectiveName — the fallback is intentionally NOT persisted
	// to config.yaml, so the on-disk file stays a record of choices
	// the user actually made.
	// Name uses `omitempty` so resetting (cfg.Name = "") produces a
	// YAML file with the `name:` line dropped entirely — matching
	// the "no name = fall back to hostname" semantics rather than
	// leaving an explicit `name: ""` in the user's config.
	Name     string         `yaml:"name,omitempty"`
	Feishu   FeishuConfig   `yaml:"feishu"`
	Telegram TelegramConfig `yaml:"telegram"`
	Slack    SlackConfig    `yaml:"slack"`
	Primary  string         `yaml:"primary"`
	Agents   []AgentEntry   `yaml:"agents"`
	Session  SessionConfig  `yaml:"session"`
	Logging  LoggingConfig  `yaml:"logging"`
	Paths    PathsConfig    `yaml:"paths"`
}

// EffectiveName returns the configured Name, or the local machine
// hostname if Name is empty. Never returns "" — callers can rely on
// a non-empty string for log fields / message headers.
//
// The hostname fallback is NOT written back to c.Name: doing so
// would silently modify the user's config.yaml on first run. If the
// caller wants the host as the "real" name, they should run
// `nightme config` → Name explicitly.
func EffectiveName(c *Config) string {
	if c != nil && c.Name != "" {
		return c.Name
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}

// FeishuConfig holds credentials for the Feishu (Lark) channel.
// AppSecret is the only sensitive field; the rest are public IDs.
type FeishuConfig struct {
	AppID             string `yaml:"app_id"`
	AppSecret         string `yaml:"app_secret"`
	VerificationToken string `yaml:"verification_token"`
	EncryptKey        string `yaml:"encrypt_key"`

	// RateLimit 控制 feishu 包内全局 token bucket（F-35）。
	// 留空 = StrictDefault（保守，RatePerSec=5, Burst=1）。
	// 调高 = 冒触顶飞书限流错误码 230001 / 230020 风险。
	RateLimit *FeishuRateLimitConfig `yaml:"rate_limit,omitempty"`
}

// FeishuRateLimitConfig 是 feishu 包内全局 token bucket 的配置。
//
// RatePerSec：每秒补充令牌数（飞书侧：50 QPS per app + 5 QPS per user /
//
//	group / message_id）。
//
// Burst：桶容量（最大突发令牌数；1 = 无突发）。
//
// 详见 docs/feat/F-35-ratelimit.md。
type FeishuRateLimitConfig struct {
	RatePerSec float64 `yaml:"rate_per_sec"`
	Burst      int     `yaml:"burst"`
}

type TelegramConfig struct {
	BotToken       string `yaml:"bot_token"`
	PollingTimeout int    `yaml:"polling_timeout"`
}

// SlackConfig holds credentials for the Slack channel.
//
// Both tokens are sensitive. BotToken (xoxb-) authenticates Web API
// calls; AppToken (xapp-, scope connections:write) opens the Socket
// Mode WebSocket. nightme only supports Socket Mode — there is no
// Events API / public-URL path, so no signing secret is needed.
//
// StreamThrottleMs is the per-turn minimum interval between
// chat.appendStream calls (docs/channel/slack.md §2.3). The default
// is deliberately conservative: Slack's Tier 4 would allow ~600ms,
// but a chat placeholder does not need that refresh rate and
// leaving headroom avoids 429-driven backoff that looks worse than
// a slower tick.
type SlackConfig struct {
	BotToken         string `yaml:"bot_token"`
	AppToken         string `yaml:"app_token"`
	StreamThrottleMs int    `yaml:"stream_throttle_ms"`

	// RateLimit controls the slack package's global token bucket.
	// Empty = StrictDefault. See docs/channel/slack.md §2.6 — the
	// bucket is global (not per-chat) because nightme runs many
	// chats in parallel and they share one Slack app quota.
	RateLimit *SlackRateLimitConfig `yaml:"rate_limit,omitempty"`
}

// SlackRateLimitConfig is the slack package's global token bucket
// configuration. RatePerSec refills per second; Burst is the bucket
// capacity (1 = no burst).
type SlackRateLimitConfig struct {
	RatePerSec float64 `yaml:"rate_per_sec"`
	Burst      int     `yaml:"burst"`
}

// AgentsConfig is REMOVED in v1.2 (post interactive-config refactor).
// The global default + recipes are now top-level fields on Config:
//   - Config.Primary  (was AgentsConfig.Default)
//   - Config.Agents   (was AgentsConfig.Recipes; was a map[string]AgentEntry
//     with yaml:",inline"; now a flat list of AgentEntry)
//
// Kept as a comment placeholder so PR reviewers see the explicit
// removal. Delete on next doc pass.

// AgentEntry is the spawn recipe for one CLI.
//
// v1.2: minimal schema — only name, bridge, command. The `command`
// field is the full command line (binary + args) as a single string,
// e.g. "claude --dangerously-skip-permissions". Args / Env from the
// previous schema were removed; users put extras in the command
// string or rely on the inherited shell environment.
type AgentEntry struct {
	// Name is the agent identifier used at spawn time and in
	// `nightme agents` listings.
	Name string `yaml:"name"`

	// Bridge selects the Bridge backend (claude / codex / opencode).
	// Names match the registered Starter's Info().Name values.
	// trigger errors at session-create time.
	Bridge string `yaml:"bridge"`

	// Command is the full command line (executable + args). Parsed
	// with shell-style splitting at spawn time, e.g.
	// `"claude --dangerously-skip-permissions"` becomes
	// []string{"claude", "--dangerously-skip-permissions"}.
	Command string `yaml:"command"`
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
	// DataDir is the root for the v1.2 persistence stores
	// (chat_sessions.json, agent_sessions.json) and per-session
	// scratch.
	DataDir string `yaml:"data_dir"`
}

// DefaultPath is the conventional location of the nightme config
// file: $HOME/.nightme/config.yaml.
//
// $NIGHTME_CONFIG overrides the location entirely (pointed at a
// non-default path for testing, a per-project override, etc.). All
// nightme state lives under $HOME/.nightme — no XDG split.
func DefaultPath() string {
	if v := os.Getenv("NIGHTME_CONFIG"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".nightme", "config.yaml")
}

// LoadDefault loads Config from DefaultPath(). If the file is missing
// it returns a Config populated entirely from defaults (no error).
//
// When cfg.Primary is still empty after Load (no YAML value, no
// env override), LoadDefault probes agent.Builtins in registration
// order — see internal/agent/registry.go — and uses the first
// Starter whose Detect() succeeds. The chosen agent name is then
// persisted via SaveDefault so subsequent starts skip probing.
//
// If no builtin is detectable, cfg.Primary stays empty and the
// caller (typically the daemon's GetOrCreate → chatstore.Bootstrap
// path) surfaces a "need primaryAgent to create" error. We do
// NOT write an empty Primary to disk on failure — leaving the
// field unset lets the user keep editing the file by hand or
// installing an agent before the next start.
//
// See docs/primary-agent-detection.md for the full rationale.
func LoadDefault() (*Config, error) {
	cfg, err := Load(DefaultPath())
	if err != nil {
		return nil, err
	}
	if cfg.Primary == "" {
		if detected := detectPrimaryFromBuiltins(); detected != "" {
			cfg.Primary = detected
			if saveErr := SaveDefault(cfg); saveErr != nil {
				// Non-fatal: the in-memory cfg is correct for this
				// process, but the next start will re-probe. Log
				// so operators can spot a misconfigured home dir
				// or read-only filesystem.
				slog.Default().Warn("config: failed to persist auto-detected primary",
					"primary", detected,
					"err", saveErr)
			}
		}
	}
	return cfg, nil
}

// detectPrimaryFromBuiltins iterates agent.Builtins in registration
// order (cmd/nightme/agents.go init) and returns the name of the
// first Starter whose Detect() reports the binary is available.
// Returns "" when nothing is detectable.
//
// Kept as a tiny pure helper so LoadDefault's control flow stays
// readable; no side effects, no logging, no I/O beyond Detect().
//
// PTY-backed starters are NOT excluded here — internal/bridge/pty
// is intentionally absent from agent.Builtins (see
// docs/primary-agent-detection.md §"Why PTY is not in Builtins"),
// so this function simply never sees them.
func detectPrimaryFromBuiltins() string {
	for _, s := range agent.Builtins.List() {
		if s == nil {
			continue
		}
		if err := s.Detect(); err == nil {
			return s.Info().Name
		}
	}
	return ""
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
//
// NOTE: cfg.Primary is intentionally NOT defaulted here. The
// default is resolved at LoadDefault time by probing
// agent.Builtins, so that the active agent reflects what the
// user actually has installed rather than a hard-coded name.
// See docs/primary-agent-detection.md.
func applyDefaults(c *Config) {
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
	// Logging.Level intentionally left empty when unset. The
	// logger resolves "" to WARN via internal/logging.levelFor
	// — the "interactive surface shouldn't leak INFO chatter"
	// default that hard-coding "info" here used to break. Users
	// who want verbose logs set cfg.Logging.Level = "info" /
	// "debug" explicitly.
	if c.Paths.DataDir == "" {
		c.Paths.DataDir = "~/.nightme"
	}
	if c.Telegram.PollingTimeout == 0 {
		c.Telegram.PollingTimeout = 30
	}
	if c.Slack.StreamThrottleMs == 0 {
		c.Slack.StreamThrottleMs = 3000
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
	if v := os.Getenv("NIGHTME_TELEGRAM_BOT_TOKEN"); v != "" {
		c.Telegram.BotToken = v
	}
	if v := os.Getenv("NIGHTME_TELEGRAM_POLLING_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Telegram.PollingTimeout = n
		}
	}
	if v := os.Getenv("NIGHTME_SLACK_BOT_TOKEN"); v != "" {
		c.Slack.BotToken = v
	}
	if v := os.Getenv("NIGHTME_SLACK_APP_TOKEN"); v != "" {
		c.Slack.AppToken = v
	}
	if v := os.Getenv("NIGHTME_SLACK_STREAM_THROTTLE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Slack.StreamThrottleMs = n
		}
	}
	if v := os.Getenv("NIGHTME_PRIMARY"); v != "" {
		c.Primary = v
	}
	if v := os.Getenv("NIGHTME_NAME"); v != "" {
		c.Name = v
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
