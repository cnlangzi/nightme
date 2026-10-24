package gtw

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/pathutil"
)

// defaultLanguage is the output language used when neither the
// repo scope nor the global scope sets one.
const defaultLanguage = "English"

// placeholderLanguage is the token the three gtw prompts carry.
// The resolved language replaces it right before the prompt
// reaches the agent — the prompts never ship the token.
const placeholderLanguage = "{{GTW_LANGUAGE}}"

// langKey is the only config key /gtw config understands today.
const langKey = "lang"

// reposKey is the top-level mapping holding the per-repository
// settings.
const reposKey = "repos"

// userConfigRelPath is the user-visible form of the config file
// path. Reply text uses it so the user sees a path they can paste
// into a shell, not an absolute path that differs per machine.
const userConfigRelPath = "~/.nightme/gtw.yml"

// langScope names where a resolved language came from. The labels
// are user-visible — they show up in the /gtw config reply.
type langScope string

const (
	scopeRepo    langScope = "repo scope"
	scopeGlobal  langScope = "global scope"
	scopeDefault langScope = "default"
)

// userConfigPath returns the path of the user-level gtw config
// (~/.nightme/gtw.yml). Load() and the write path share it so the
// two can never disagree about which file they mean.
func userConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return pathutil.Join(home, ".nightme", "gtw.yml"), nil
}

// repoKey returns the "host/owner/repo" key of the repository dir
// belongs to, derived from its origin remote URL. The write path
// and the read path both go through this one function, so a value
// written for a repository is found again by the same key.
//
// Returns an error when the repository has no origin remote or the
// URL shape is not recognised — callers decide whether that is
// fatal (a repo-scope write) or a fallback (resolving the
// effective language).
func repoKey(ctx context.Context, dir string, git GitRunner) (string, error) {
	if git == nil {
		return "", errors.New("gtw: no git runner")
	}
	remoteURL, err := RemoteOriginURL(ctx, dir, git)
	if err != nil {
		return "", fmt.Errorf("no origin remote: %w", err)
	}
	host, err := parseRemoteHost(remoteURL)
	if err != nil {
		return "", err
	}
	owner, repo, err := ParseRepoOwner(remoteURL)
	if err != nil {
		return "", err
	}
	return host + "/" + owner + "/" + repo, nil
}

// resolveLanguageScope resolves the effective output language and
// the scope it came from: repo scope > global scope > default.
//
// A repository whose origin remote cannot be read simply has no
// repo-scope entry — the resolution falls through to the global
// scope instead of failing. Resolving the language must never
// block the command that asked for it. An empty dir means "no
// repository known" and skips the repo scope entirely.
func resolveLanguageScope(ctx context.Context, cfg Config, dir string, git GitRunner) (string, langScope) {
	if dir != "" {
		if key, err := repoKey(ctx, dir, git); err == nil && key != "" {
			if repo, ok := cfg.Repos[key]; ok {
				if v := strings.TrimSpace(repo.Lang); v != "" {
					return v, scopeRepo
				}
			}
		}
	}
	if v := strings.TrimSpace(cfg.Lang); v != "" {
		return v, scopeGlobal
	}
	return defaultLanguage, scopeDefault
}

// resolveLanguage returns the effective output language — the one
// the commit / pr / new prompts are written in.
func resolveLanguage(ctx context.Context, cfg Config, dir string, git GitRunner) string {
	lang, _ := resolveLanguageScope(ctx, cfg, dir, git)
	return lang
}

// applyLanguage replaces the language placeholder in a prompt with
// the resolved language. An empty language falls back to the
// default so a prompt never ships with an empty slot.
func applyLanguage(text, lang string) string {
	if strings.TrimSpace(lang) == "" {
		lang = defaultLanguage
	}
	return strings.ReplaceAll(text, placeholderLanguage, lang)
}

// --- gtw.yml write path ---
//
// The write path edits the YAML document at the node level so
// hand-written hooks, comments, and unknown keys survive the
// round-trip. A file that does not parse is never rewritten — a
// broken user config is reported, not overwritten.

// writeUserConfig applies mutate to the parsed ~/.nightme/gtw.yml
// and writes the result back. A missing file starts from an empty
// mapping; a malformed file is an error and the file is left
// untouched.
func writeUserConfig(mutate func(root *yaml.Node) error) error {
	path, err := userConfigPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	root, err := parseConfigRoot(data, path)
	if err != nil {
		return err
	}
	if err := mutate(root); err != nil {
		return err
	}
	out, err := encodeConfigRoot(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(pathutil.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, out)
}

// writeFileAtomic writes data to path via a temp file in the same
// directory followed by a rename. A failed write leaves the
// previous file untouched — the user's hand-written config never
// ends up truncated.
func writeFileAtomic(path string, data []byte) error {
	dir := pathutil.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// parseConfigRoot parses the YAML document into its root mapping
// node. An empty file yields a fresh empty mapping; a document
// whose root is not a mapping is rejected — writing into it would
// destroy whatever the user meant.
func parseConfigRoot(data []byte, path string) (*yaml.Node, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("%s: not a YAML mapping", path)
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: not a YAML mapping", path)
	}
	return root, nil
}

// encodeConfigRoot serializes a root mapping back to YAML. The
// indent is pinned to 2 spaces to match the shape users hand-write.
func encodeConfigRoot(root *yaml.Node) ([]byte, error) {
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// mapValue returns the value node stored under key in a mapping, or
// nil when the mapping has no such key.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setMapValue stores a string scalar under key, creating the key
// when the mapping does not have it yet.
func setMapValue(m *yaml.Node, key, value string) {
	if v := mapValue(m, key); v != nil {
		v.Kind = yaml.ScalarNode
		v.Tag = "!!str"
		v.Value = value
		v.Content = nil
		return
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

// deleteMapKey removes key from a mapping. Reports whether the key
// was there.
func deleteMapKey(m *yaml.Node, key string) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return true
		}
	}
	return false
}

// setGlobalLang writes the user-scope output language.
func setGlobalLang(lang string) error {
	return writeUserConfig(func(root *yaml.Node) error {
		setMapValue(root, langKey, lang)
		return nil
	})
}

// unsetGlobalLang removes the user-scope output language.
func unsetGlobalLang() error {
	return writeUserConfig(func(root *yaml.Node) error {
		deleteMapKey(root, langKey)
		return nil
	})
}

// setRepoLang writes the repo-scope output language for repo.
func setRepoLang(repo, lang string) error {
	return writeUserConfig(func(root *yaml.Node) error {
		repos := mapValue(root, reposKey)
		if repos == nil {
			repos = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			root.Content = append(root.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: reposKey},
				repos)
		}
		if repos.Kind != yaml.MappingNode {
			return fmt.Errorf("`%s` is not a mapping", reposKey)
		}
		entry := mapValue(repos, repo)
		if entry == nil {
			entry = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			repos.Content = append(repos.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: repo},
				entry)
		}
		if entry.Kind != yaml.MappingNode {
			return fmt.Errorf("`%s.%s` is not a mapping", reposKey, repo)
		}
		setMapValue(entry, langKey, lang)
		return nil
	})
}

// unsetRepoLang removes the repo-scope output language for repo.
// Empty containers left behind are dropped so the file does not
// accumulate `repos: {}` shells.
func unsetRepoLang(repo string) error {
	return writeUserConfig(func(root *yaml.Node) error {
		repos := mapValue(root, reposKey)
		if repos == nil || repos.Kind != yaml.MappingNode {
			return nil
		}
		entry := mapValue(repos, repo)
		if entry == nil || entry.Kind != yaml.MappingNode {
			return nil
		}
		deleteMapKey(entry, langKey)
		if len(entry.Content) == 0 {
			deleteMapKey(repos, repo)
		}
		if len(repos.Content) == 0 {
			deleteMapKey(root, reposKey)
		}
		return nil
	})
}

// --- /gtw config ---

var configCmdSpec = command.CmdSpec{
	Name:  "/gtw config",
	Usage: "/gtw config [-g|--global] [--unset] lang [value]",
	Flags: map[string]command.FlagSpec{
		"-g":       {Name: "global"},
		"--global": {Name: "global"},
		"--unset":  {Name: "unset"},
	},
	MinArgs: 1,
	// The value is free text and may carry spaces ("Simplified
	// Chinese"), so the tail after the key is one value rather
	// than a fixed number of positionals.
	MaxArgs: command.UnboundedArgs,
}

// trimQuotes drops one matching pair of surrounding double quotes
// from a value. The slash-command pipeline splits argv on
// whitespace without quote awareness, so a quoted value arrives
// with its quotes still attached.
func trimQuotes(s string) string {
	if len(s) >= 2 && strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"") {
		return s[1 : len(s)-1]
	}
	return s
}

// RunConfig implements /gtw config — the git-style reader /
// writer for the output language.
//
// Four forms:
//
//	/gtw config lang <value>       write the repo-scope language
//	/gtw config -g lang <value>    write the user-scope language
//	/gtw config lang               show the effective language
//	/gtw config --unset lang       remove the entry (add -g for
//	                                the user scope)
//
// The language value is free text — any language name the model
// understands. An empty value counts as "not set", so it removes
// the entry instead of writing one.
func RunConfig(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
	argv []string,
) (*Result, error) {
	parsed, err := command.ParseCmdArgs(argv, configCmdSpec)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID, "❌ "+err.Error()), nil
	}

	key := parsed.Arg(0)
	if key != langKey {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ unknown config key: %s\n  known keys: %s", key, langKey)), nil
	}

	global := parsed.Bool("global")
	unset := parsed.Bool("unset")
	hasValue := parsed.NArgs() >= 2
	// Everything after the key is the value: the language is free
	// text and may carry spaces.
	value := trimQuotes(strings.Join(parsed.Args[1:], " "))
	value = strings.TrimSpace(value)

	if unset && hasValue {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ --unset takes the key only: "+configCmdSpec.Usage), nil
	}
	// An empty value is "not set" — it removes the entry instead of
	// writing an empty one.
	if hasValue && value == "" {
		unset = true
		hasValue = false
	}

	// Query — no write, no repo key required.
	if !unset && !hasValue {
		return configQuery(ctx, cs, deps, chatID, messageID, global)
	}

	if global {
		if unset {
			if err := unsetGlobalLang(); err != nil {
				return reply(ctx, cs.Emitter(), chatID, messageID,
					fmt.Sprintf("❌ %v", err)), nil
			}
			return reply(ctx, cs.Emitter(), chatID, messageID,
				"✅ gtw.lang unset (global scope)\n"+
					"→ file: "+userConfigRelPath), nil
		}
		if err := setGlobalLang(value); err != nil {
			return reply(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ %v", err)), nil
		}
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("✅ gtw.lang = %s (global scope)", value)+
				"\n→ file: "+userConfigRelPath), nil
	}

	// Repo scope — needs the repository key, which comes from the
	// origin remote of the chat's cwd.
	cwd := cs.SelectedCwd()
	if cwd == "" {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ no active workspace. Send /cwd <path> first."), nil
	}
	repo, err := repoKey(ctx, cwd, deps.Git)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ cannot determine the repository key: %v\n"+
				"  hint: use `/gtw config -g %s <value>` for the user-level scope", err, langKey)), nil
	}
	if unset {
		if err := unsetRepoLang(repo); err != nil {
			return reply(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ %v", err)), nil
		}
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"✅ gtw.lang unset (repo scope)\n"+
				"→ file: "+userConfigRelPath+
				"\n→ repo: "+repo), nil
	}
	if err := setRepoLang(repo, value); err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ %v", err)), nil
	}
	return reply(ctx, cs.Emitter(), chatID, messageID,
		fmt.Sprintf("✅ gtw.lang = %s (repo scope)", value)+
			"\n→ file: "+userConfigRelPath+
			"\n→ repo: "+repo), nil
}

// configQuery reports the effective output language and the scope
// it came from. Without -g the effective language is resolved
// across both scopes; with -g only the user scope is inspected.
func configQuery(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
	global bool,
) (*Result, error) {
	cfg, notes := Load()
	warnings := ""
	if notes.HasWarnings() {
		warnings = strings.Join(notes.Warnings, "\n") + "\n"
	}

	if global {
		lang := strings.TrimSpace(cfg.Lang)
		if lang == "" {
			lang = defaultLanguage
			return reply(ctx, cs.Emitter(), chatID, messageID,
				"✨ gtw.lang = "+lang+" ("+string(scopeDefault)+")\n"+warnings), nil
		}
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"✨ gtw.lang = "+lang+" ("+string(scopeGlobal)+")\n"+warnings), nil
	}

	cwd := cs.SelectedCwd()
	lang, scope := resolveLanguageScope(ctx, cfg, cwd, deps.Git)
	return reply(ctx, cs.Emitter(), chatID, messageID,
		"✨ gtw.lang = "+lang+" ("+string(scope)+")\n"+warnings), nil
}
