// model.go — the model opencode's own configuration selects.
//
// opencode resolves its model per session, and the model the user's
// configuration selects never reaches an ACP session: `opencode acp`
// opens every session on opencode's own choice. The bridge asks
// opencode which model its configuration selects (`opencode debug
// config`) and applies that model to the session it opens. The model
// is the user's — the bridge never picks one, and never writes to
// the user's opencode configuration.
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/bridge/acp"
	"github.com/cnlangzi/nightme/internal/proc"
)

// modelConfigOption is the ACP config option id opencode exposes the
// session model under.
const modelConfigOption = "model"

// configLookupTimeout bounds the `opencode debug config` call that
// reads the effective model. The command answers in a fraction of a
// second; the budget only keeps a hung CLI from stalling the spawn.
const configLookupTimeout = 15 * time.Second

// effectiveModel returns the model opencode's own configuration
// selects for the workspace — "providerID/model", plus "/variant"
// when the configuration carries one. Empty when the configuration
// selects no model.
//
// The lookup runs in the workspace, so a project-level configuration
// is part of the answer.
func effectiveModel(ctx context.Context, command, workspace string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, configLookupTimeout)
	defer cancel()
	cmd := proc.New(ctx, command, "debug", "config")
	cmd.Dir = workspace
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("opencode: debug config: %w%s", err, commandStderr(err))
	}
	return parseEffectiveModel(out), nil
}

// commandStderr renders what the command printed on stderr, so a
// failed lookup says why.
func commandStderr(err error) string {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return ""
	}
	text := strings.TrimSpace(string(exitErr.Stderr))
	if text == "" {
		return ""
	}
	return ": " + text
}

// applyConfiguredModel returns the pre-Ready hook that puts the
// model opencode's configuration selects on the session. It runs
// after session/new answered and before the first prompt, so no turn
// ever runs on a model the user didn't configure.
//
// The lookup runs here rather than alongside the spawn: opencode's
// acp server and its background service share one SQLite database,
// and a second opencode process during the handshake is what makes
// session/new lose the lock and fail.
//
// Failure is soft: the session stays open on opencode's own model
// and the spawn succeeds.
func (s *Starter) applyConfiguredModel(ctx context.Context, workspace string) acp.OnSessionID {
	return func(d *acp.DriverHandle, _ string) error {
		model, err := effectiveModel(ctx, s.command, workspace)
		if err != nil {
			oLog("effective model lookup failed", "err", err)
			return nil
		}
		if model == "" {
			return nil
		}
		if _, err := d.SetConfigOption(modelConfigOption, model); err != nil {
			oLog("apply model failed", "model", model, "err", err)
			return nil
		}
		oLog("model applied", "model", model)
		return nil
	}
}

// configSource is one entry of the configuration-source list
// `opencode debug config` prints. Only "document" entries carry a
// parsed configuration; "directory" entries point at a directory
// opencode scans.
type configSource struct {
	Type string          `json:"type"`
	Info json.RawMessage `json:"info"`
}

// parseEffectiveModel extracts the effective model from the JSON
// `opencode debug config` prints. The sources are listed in
// precedence order, so the last document carrying a model wins.
func parseEffectiveModel(data []byte) string {
	sources, ok := decodeConfigSources(data)
	if !ok {
		return ""
	}
	model := ""
	for _, source := range sources {
		if source.Type != "document" {
			continue
		}
		if m := modelFromConfigDocument(source.Info); m != "" {
			model = m
		}
	}
	return model
}

// decodeConfigSources parses the configuration-source list. The
// command prints bare JSON; a stray line around it is tolerated by
// falling back to the array slice.
func decodeConfigSources(data []byte) ([]configSource, bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, false
	}
	var sources []configSource
	if json.Unmarshal(trimmed, &sources) == nil {
		return sources, true
	}
	start := bytes.IndexByte(trimmed, '[')
	end := bytes.LastIndexByte(trimmed, ']')
	if start < 0 || end <= start {
		return nil, false
	}
	if json.Unmarshal(trimmed[start:end+1], &sources) != nil {
		return nil, false
	}
	return sources, true
}

// modelFromConfigDocument reads the "model" key of one configuration
// document.
func modelFromConfigDocument(info json.RawMessage) string {
	if len(info) == 0 {
		return ""
	}
	var doc struct {
		Model json.RawMessage `json:"model"`
	}
	if json.Unmarshal(info, &doc) != nil {
		return ""
	}
	return modelValue(doc.Model)
}

// modelValue renders a configuration model value as the ACP config
// option value: "providerID/model", plus "/variant" when the
// configuration carries one. opencode's ACP server reads the variant
// as the last path segment of the option value.
//
// opencode normalizes the configuration key to the object form; the
// plain "provider/model" string is accepted too.
func modelValue(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	var provider, model, variant string
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) != nil {
			return ""
		}
		provider, model, variant = splitModelID(s)
	} else {
		var obj struct {
			ProviderID string `json:"providerID"`
			Model      string `json:"model"`
			Variant    string `json:"variant"`
		}
		if json.Unmarshal(trimmed, &obj) != nil {
			return ""
		}
		provider, model, variant = obj.ProviderID, obj.Model, obj.Variant
	}
	if provider == "" || model == "" {
		return ""
	}
	value := provider + "/" + model
	if variant != "" {
		value += "/" + variant
	}
	return value
}

// splitModelID splits a "provider/model" id with an optional
// "#variant" suffix into its parts.
func splitModelID(s string) (provider, model, variant string) {
	if i := strings.Index(s, "#"); i >= 0 {
		s, variant = s[:i], s[i+1:]
	}
	i := strings.Index(s, "/")
	if i < 0 {
		return "", "", ""
	}
	return s[:i], s[i+1:], variant
}
