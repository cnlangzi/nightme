package llm

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Config is the configuration of one LLM capability.
//
// The same shape serves both capabilities: a chat provider is named by
// Provider, a decision protocol by Protocol. The user configures the
// model; nightme never picks one.
type Config struct {
	// Provider names a registered chat provider ("openai").
	Provider string `json:"provider,omitempty" yaml:"provider,omitempty"`
	// Protocol names a registered decision protocol ("decisions",
	// "systemone").
	Protocol string `json:"protocol,omitempty" yaml:"protocol,omitempty"`
	// BaseURL is the root of the API.
	BaseURL string `json:"base_url,omitempty" yaml:"base_url,omitempty"`
	// Model is the model to call.
	Model string `json:"model,omitempty" yaml:"model,omitempty"`
	// APIKeyEnv names the environment variable the API key is read
	// from. The key is never stored in the configuration.
	APIKeyEnv string `json:"api_key_env,omitempty" yaml:"api_key_env,omitempty"`
	// MaxTokens caps the length of an answer.
	MaxTokens int `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
}

// Factory builds a chat provider from a configuration.
type Factory func(cfg Config) (Provider, error)

// DeciderFactory builds a decider from a configuration.
type DeciderFactory func(cfg Config) (Decider, error)

var (
	registryMu sync.RWMutex
	providers  = map[string]Factory{}
	deciders   = map[string]DeciderFactory{}
)

// Register adds a chat provider under name. A second registration of
// the same name replaces the first, so a test can install a fake.
//
// The concrete protocols register themselves from their package's init.
func Register(name string, f Factory) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || f == nil {
		return
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	providers[name] = f
}

// RegisterDecider adds a decision protocol under name. A second
// registration of the same name replaces the first.
func RegisterDecider(name string, f DeciderFactory) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || f == nil {
		return
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	deciders[name] = f
}

// New builds the chat provider the configuration names.
//
// The configuration is validated before the provider is built: an
// unknown provider, a missing model or a missing API key environment
// variable is an error the caller can report.
func New(cfg Config) (Provider, error) {
	name := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if name == "" {
		return nil, fmt.Errorf("llm: no provider configured")
	}
	registryMu.RLock()
	f, ok := providers[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("llm: unknown provider %q (known: %s)", name, strings.Join(ProviderNames(), ", "))
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("llm: provider %q has no model", name)
	}
	return f(cfg)
}

// NewDecider builds the decider the configuration names.
func NewDecider(cfg Config) (Decider, error) {
	name := strings.ToLower(strings.TrimSpace(cfg.Protocol))
	if name == "" {
		return nil, fmt.Errorf("llm: no decision protocol configured")
	}
	registryMu.RLock()
	f, ok := deciders[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("llm: unknown decision protocol %q (known: %s)", name, strings.Join(DeciderNames(), ", "))
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("llm: decision protocol %q has no model", name)
	}
	return f(cfg)
}

// ProviderNames lists the registered chat providers, sorted.
func ProviderNames() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(providers))
	for name := range providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// DeciderNames lists the registered decision protocols, sorted.
func DeciderNames() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(deciders))
	for name := range deciders {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
