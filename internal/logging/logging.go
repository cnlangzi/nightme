// Package logging wraps log/slog with a small opinionated factory:
//
//   - JSON output (structured, machine-friendly).
//   - File destination at cfg.Logging.File, falling back to
//     ~/.local/share/nightme/nightme.log, mode 0600.
//   - Secret redaction: any attribute whose key (case-insensitive)
//     contains "secret", "token", or "password" is replaced with
//     "***REDACTED***" before it reaches the handler.
//
// v0.1 keeps the surface tiny — a single New() constructor plus a
// CLI-friendly Setup helper. The handler chain is wrapped so future
// versions can add rotation, sampling, or remote export without
// touching call sites.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cnlangzi/nightme/internal/config"
)

// RedactedValue is the placeholder substituted for any attribute
// whose key matches one of the redaction patterns.
const RedactedValue = "***REDACTED***"

// redactedKeys is the case-insensitive substring list. Adding a
// pattern here is sufficient to redact a new field — no need to
// touch call sites.
var redactedKeys = []string{"secret", "token", "password"}

// redactingHandler wraps a slog.Handler and rewrites suspicious
// attributes. It implements slog.Handler so it composes with any
// underlying JSON / text handler.
type redactingHandler struct {
	inner slog.Handler
}

func (h *redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	// Records store attrs in an immutable slice; rewrite by
	// inspecting each and replacing in place when needed.
	filtered := make([]slog.Attr, 0, r.NumAttrs())
	redacted := false
	r.Attrs(func(a slog.Attr) bool {
		if shouldRedact(a.Key) {
			a.Value = slog.StringValue(RedactedValue)
			redacted = true
		}
		filtered = append(filtered, a)
		return true
	})
	if redacted {
		// Build a fresh record with the rewritten attrs.
		r = slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
		r.AddAttrs(filtered...)
	}
	return h.inner.Handle(ctx, r)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		if shouldRedact(a.Key) {
			a.Value = slog.StringValue(RedactedValue)
		}
		newAttrs[i] = a
	}
	return &redactingHandler{inner: h.inner.WithAttrs(newAttrs)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{inner: h.inner.WithGroup(name)}
}

// shouldRedact reports whether a key matches any redaction pattern.
// Match is case-insensitive substring — broad enough to catch
// AppSecret, access_token, db_password, etc.
func shouldRedact(key string) bool {
	k := strings.ToLower(key)
	for _, p := range redactedKeys {
		if strings.Contains(k, p) {
			return true
		}
	}
	return false
}

// levelFor resolves the slog.Level for a config string. Unknown
// values fall back to info and never error — logging should not
// block process startup.
func levelFor(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// resolvePath returns the absolute log file path. Empty cfg path
// falls back to ~/.local/share/nightme/nightme.log.
func resolvePath(cfg *config.Config) (string, error) {
	p := ""
	if cfg != nil {
		p = strings.TrimSpace(cfg.Logging.File)
	}
	if p == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("logging: resolve home dir: %w", err)
		}
		p = filepath.Join(home, ".local", "share", "nightme", "nightme.log")
	}
	return p, nil
}

// New constructs a JSON slog.Logger that writes to cfg.Logging.File
// (or the default fallback). The file is created with mode 0600 so
// accidental reads from other users are blocked (NFR N-7).
//
// When level is unset, info is used. The returned logger never
// returns nil — if file creation fails, errors surface here.
func New(cfg *config.Config) (*slog.Logger, error) {
	path, err := resolvePath(cfg)
	if err != nil {
		return nil, err
	}

	level := slog.LevelInfo
	if cfg != nil {
		level = levelFor(cfg.Logging.Level)
	}

	f, err := openLogFile(path)
	if err != nil {
		return nil, fmt.Errorf("logging: open %s: %w", path, err)
	}

	jsonHandler := slog.NewJSONHandler(f, &slog.HandlerOptions{
		Level: level,
	})
	handler := &redactingHandler{inner: jsonHandler}
	return slog.New(handler), nil
}

// openLogFile creates the parent directory (0700) and the file
// itself (0600). It returns an open *os.File the caller owns.
func openLogFile(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("logging: mkdir %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("logging: open file: %w", err)
	}
	return f, nil
}

// Setup is a convenience wrapper around New() that swaps the
// default logger. It is intended for cobra PersistentPreRun; tests
// use New() directly.
func Setup(cfg *config.Config) error {
	lg, err := New(cfg)
	if err != nil {
		return err
	}
	slog.SetDefault(lg)
	return nil
}

// Discard returns a no-op logger useful in tests that need to
// satisfy a constructor without writing anywhere.
func Discard() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}
