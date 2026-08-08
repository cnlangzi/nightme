package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cnlangzi/nightme/internal/config"
)

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
		p = filepath.Join(home, ".nightme", "nightme.log")
	}
	return p, nil
}

// Path returns the resolved log file path for cfg, honoring an
// explicit Logging.File and falling back to the default at
// $HOME/.nightme/nightme.log. Exposed so companion commands
// (notably `nightme logs`) can locate the same file the logger
// writes to without duplicating the resolution logic.
func Path(cfg *config.Config) (string, error) {
	return resolvePath(cfg)
}

type closeableHandler struct {
	slog.Handler
	file *os.File
}

func (h *closeableHandler) Close() error { return h.file.Close() }
func (h *closeableHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &closeableHandler{Handler: h.Handler.WithAttrs(attrs), file: h.file}
}
func (h *closeableHandler) WithGroup(name string) slog.Handler {
	return &closeableHandler{Handler: h.Handler.WithGroup(name), file: h.file}
}

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
	// Tee log lines to the file, stdout, and stderr so the CLI
	// surface shows the same trace the persisted log captures,
	// irrespective of which stream the user redirects. Without
	// the stdout/sderr sinks the user has to tail the log file
	// to see what nightme is doing — useful for debugging which
	// stage dropped a message but painful during a live session.
	//
	// Three sinks is defensive: stderr matches the lark SDK's
	// existing CLI output and systemd/journald capture it by
	// default; stdout is the conventional user-requested sink;
	// combined they survive `> log.txt` and `2> log.txt`
	// redirections without losing the trace. The cost is one
	// extra write per log line — negligible at nightme's log
	// volume (single-digit messages per minute).
	sink := io.MultiWriter(f, os.Stdout, os.Stderr)
	handler := &closeableHandler{Handler: slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: level}), file: f}
	return slog.New(handler), nil
}

func Close(logger *slog.Logger) error {
	if logger == nil {
		return nil
	}
	if closer, ok := logger.Handler().(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

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

func Setup(cfg *config.Config) error {
	lg, err := New(cfg)
	if err != nil {
		return err
	}
	slog.SetDefault(lg)
	return nil
}

func Discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

var _ slog.Handler = (*closeableHandler)(nil)
var _ io.Closer = (*closeableHandler)(nil)
