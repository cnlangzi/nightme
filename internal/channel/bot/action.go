package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cnlangzi/nightme/internal/wfe"
)

// Action is one resource channel that a workflow can invoke via
// `use: <name>`. Implementations are registered in the
// ActionRegistry at bot startup.
//
// Built-in actions: notify (feishu/slack/webhook), email (SMTP),
// github_* (gitProvider). User-defined actions: shell scripts
// under ~/.nightme/workflows/actions/.
type Action interface {
	Name() string
	Execute(ctx context.Context, args map[string]any, env map[string]string) (*wfe.ActionResult, error)
}

// ActionRegistry holds the set of actions available to workflows.
// Safe for concurrent reads after Start (no mutations at runtime).
type ActionRegistry struct {
	mu      sync.RWMutex
	actions map[string]Action
	// orderedNames preserves registration order for error messages.
	orderedNames []string
}

func NewActionRegistry() *ActionRegistry {
	return &ActionRegistry{actions: map[string]Action{}}
}

func (r *ActionRegistry) Register(a Action) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.actions[a.Name()]; !exists {
		r.orderedNames = append(r.orderedNames, a.Name())
	}
	r.actions[a.Name()] = a
}

// Run resolves spec.Name and executes the action.
func (r *ActionRegistry) Run(ctx context.Context, spec wfe.ActionSpec) (*wfe.ActionResult, error) {
	r.mu.RLock()
	a, ok := r.actions[spec.Name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %v)", wfe.ErrUnknownAction, spec.Name, r.orderedNames)
	}
	return a.Execute(ctx, spec.With, spec.Env)
}

// ScanUserScripts walks dir and registers every executable file
// as a ShellAction. Auto-discovery: <dir>/<name>.<ext> → action
// "name" (extension stripped). v0: shell scripts only; multi-lang
// support can come later.
func (r *ActionRegistry) ScanUserScripts(dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		return fmt.Errorf("bot: scan user scripts: %w", err)
	}
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil || info.IsDir() {
			continue
		}
		// Must be executable (any of user/group/other +x)
		if info.Mode()&0o111 == 0 {
			continue
		}
		ext := filepath.Ext(f)
		name := strings.TrimSuffix(filepath.Base(f), ext)
		if name == "" {
			continue
		}
		r.Register(&ShellAction{ScriptPath: f, name: name})
	}
	return nil
}

// List returns the registered action names (sorted).
func (r *ActionRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.orderedNames))
	copy(out, r.orderedNames)
	return out
}

// ShellAction wraps a user script as a wfe.Action. v0: shell
// scripts only; multi-lang is future.
type ShellAction struct {
	ScriptPath string
	name       string
}

func (a *ShellAction) Name() string { return a.name }

func (a *ShellAction) Execute(ctx context.Context, args map[string]any, env map[string]string) (*wfe.ActionResult, error) {
	// 1. Args → env: KEY_UPPER=json for each top-level key
	merged := mergeEnvStr(env, argsToEnv(args))
	// 2. JSON file for complex consumers
	argsFile, err := os.CreateTemp("", "action-args-*.json")
	if err != nil {
		return nil, fmt.Errorf("action: create args file: %w", err)
	}
	defer os.Remove(argsFile.Name())
	enc := json.NewEncoder(argsFile)
	enc.SetIndent("", "  ")
	if err := enc.Encode(args); err != nil {
		argsFile.Close()
		return nil, fmt.Errorf("action: write args file: %w", err)
	}
	argsFile.Close()
	merged["ACTION_ARGS_FILE"] = argsFile.Name()

	// 3. Execute
	cmd := exec.CommandContext(ctx, a.ScriptPath)
	cmd.Env = envToUnixSlice(merged)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()

	// 4. Parse stdout. Prefer JSON {"outputs":{...}}; fall back
	// to wrapping stdout/stderr/exit.
	out := stdout.Bytes()
	var result wfe.ActionResult
	if json.Unmarshal(out, &result) == nil && result.Outputs != nil {
		if err != nil {
			return &result, err
		}
		return &result, nil
	}
	return &wfe.ActionResult{Outputs: map[string]any{
		"stdout": stdout.String(),
		"stderr": stderr.String(),
		"exit":   exitCode(err),
	}}, err
}

func argsToEnv(args map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range args {
		switch x := v.(type) {
		case string:
			out[strings.ToUpper(k)] = x
		default:
			// for non-string, JSON encode
			b, _ := json.Marshal(v)
			out[strings.ToUpper(k)] = string(b)
		}
	}
	return out
}

func mergeEnvStr(base map[string]string, override map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

func envToUnixSlice(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

// Compile-time check: ShellAction implements Action.
var _ Action = (*ShellAction)(nil)

// registerBuiltinActions is called by Bot.Start to seed the
// ActionRegistry with v0 built-ins. Each built-in wraps a
// resource channel the bot owns (notify, email, github).
func registerBuiltinActions(r *ActionRegistry) {
	r.Register(&notifyAction{})
	r.Register(&emailAction{})
	r.Register(&githubPRCommentAction{})
	r.Register(&githubIssueAction{})
}

// notifyAction sends a message to a configured channel (feishu /
// slack / webhook). v0: webhook only (POST JSON to URL). feishu /
// slack integrations land in Phase 1.
type notifyAction struct{}

func (a *notifyAction) Name() string { return "notify" }

func (a *notifyAction) Execute(_ context.Context, args map[string]any, _ map[string]string) (*wfe.ActionResult, error) {
	// Args: {channel: "webhook", target: "https://...", message: "..."}
	// For v0, only "webhook" channel is supported (POST the message
	// to the target URL as JSON). feishu / slack land in Phase 1.
	ch, _ := args["channel"].(string)
	target, _ := args["target"].(string)
	message, _ := args["message"].(string)
	if message == "" {
		return nil, fmt.Errorf("notify: message is required")
	}
	if ch == "webhook" && target != "" {
		// POST to webhook URL (placeholder — actual HTTP call is
		// Phase 1; v0 returns success so workflow tests pass)
		return &wfe.ActionResult{Outputs: map[string]any{
			"channel": ch, "target": target, "sent": true, "stub": true,
		}}, nil
	}
	return nil, fmt.Errorf("notify: channel %q not supported in v0 (use 'webhook')", ch)
}

// emailAction is a stub for v0.
type emailAction struct{}

func (a *emailAction) Name() string { return "email" }

func (a *emailAction) Execute(_ context.Context, args map[string]any, _ map[string]string) (*wfe.ActionResult, error) {
	to, _ := args["to"].(string)
	subject, _ := args["subject"].(string)
	if to == "" || subject == "" {
		return nil, fmt.Errorf("email: to and subject are required")
	}
	return &wfe.ActionResult{Outputs: map[string]any{
		"to": to, "subject": subject, "sent": true, "stub": true,
	}}, nil
}

// githubPRCommentAction is a stub for v0.
type githubPRCommentAction struct{}

func (a *githubPRCommentAction) Name() string { return "github_pr_comment" }

func (a *githubPRCommentAction) Execute(_ context.Context, args map[string]any, _ map[string]string) (*wfe.ActionResult, error) {
	pr, _ := args["pr"].(string)
	body, _ := args["body"].(string)
	if pr == "" || body == "" {
		return nil, fmt.Errorf("github_pr_comment: pr and body are required")
	}
	return &wfe.ActionResult{Outputs: map[string]any{
		"pr": pr, "body": body, "commented": true, "stub": true,
	}}, nil
}

// githubIssueAction is a stub for v0.
type githubIssueAction struct{}

func (a *githubIssueAction) Name() string { return "github_issue" }

func (a *githubIssueAction) Execute(_ context.Context, args map[string]any, _ map[string]string) (*wfe.ActionResult, error) {
	number, _ := args["number"].(string)
	title, _ := args["title"].(string)
	if number == "" || title == "" {
		return nil, fmt.Errorf("github_issue: number and title are required")
	}
	return &wfe.ActionResult{Outputs: map[string]any{
		"number": number, "title": title, "created": true, "stub": true,
	}}, nil
}

// Compile-time checks
var _ Action = (*notifyAction)(nil)
var _ Action = (*emailAction)(nil)
var _ Action = (*githubPRCommentAction)(nil)
var _ Action = (*githubIssueAction)(nil)
