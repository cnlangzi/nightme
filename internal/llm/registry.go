package llm

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ChatConfig is the configuration of the chat model (System 2).
//
// The protocol names the wire format the request speaks and the base
// URL is the root of the API: the client appends the path of its own
// protocol and nothing else, so the address the vendor's documentation
// carries is the address the request goes to.
type ChatConfig struct {
	// Protocol names a registered chat protocol ("chat_completions",
	// "messages").
	Protocol string `json:"protocol,omitempty" yaml:"protocol,omitempty"`
	// BaseURL is the root of the API.
	BaseURL string `json:"base_url,omitempty" yaml:"base_url,omitempty"`
	// Model is the model to call.
	Model string `json:"model,omitempty" yaml:"model,omitempty"`
	// APIKey holds the API key in plaintext. It wins over APIKeyEnv
	// when both are set.
	//
	// The field is tagged out of every serializer: a credential must
	// not leave the package through a status dump or a log line.
	APIKey string `json:"-" yaml:"-"`
	// APIKeyEnv names the environment variable the API key is read
	// from when APIKey is empty.
	APIKeyEnv string `json:"api_key_env,omitempty" yaml:"api_key_env,omitempty"`
	// MaxTokens caps the length of an answer.
	MaxTokens int `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	// Timeout is the deadline of one call. Zero means the default of
	// the protocol.
	Timeout time.Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	// Headers are the extra headers every request carries.
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
}

// inlineAPIKey returns the plaintext key the configuration carries.
func (c ChatConfig) inlineAPIKey() string { return strings.TrimSpace(c.APIKey) }

// apiKeyEnvName returns the environment variable the configuration
// names.
func (c ChatConfig) apiKeyEnvName() string { return strings.TrimSpace(c.APIKeyEnv) }

// DeciderConfig is the configuration of the decision model (System 1).
//
// It has the same shape as the chat model's: a protocol that names
// the wire format, a base URL that is used as it is written, a model
// and a credential.
type DeciderConfig struct {
	// Protocol names a registered decision protocol ("decisions",
	// "systemone").
	Protocol string `json:"protocol,omitempty" yaml:"protocol,omitempty"`
	// BaseURL is the root of the API.
	BaseURL string `json:"base_url,omitempty" yaml:"base_url,omitempty"`
	// Model is the model to call.
	Model string `json:"model,omitempty" yaml:"model,omitempty"`
	// APIKey holds the API key in plaintext. It wins over APIKeyEnv
	// when both are set.
	//
	// The field is tagged out of every serializer: a credential must
	// not leave the package through a status dump or a log line.
	APIKey string `json:"-" yaml:"-"`
	// APIKeyEnv names the environment variable the API key is read
	// from when APIKey is empty.
	APIKeyEnv string `json:"api_key_env,omitempty" yaml:"api_key_env,omitempty"`
	// Timeout is the deadline of one call. Zero means the default of
	// the protocol.
	Timeout time.Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	// Headers are the extra headers every request carries.
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
}

// inlineAPIKey returns the plaintext key the configuration carries.
func (c DeciderConfig) inlineAPIKey() string { return strings.TrimSpace(c.APIKey) }

// apiKeyEnvName returns the environment variable the configuration
// names.
func (c DeciderConfig) apiKeyEnvName() string { return strings.TrimSpace(c.APIKeyEnv) }

// Factory builds a chat provider from a configuration.
type Factory func(cfg ChatConfig) (Provider, error)

// DeciderFactory builds a decider from a configuration.
type DeciderFactory func(cfg DeciderConfig) (Decider, error)

// Protocol is a registered wire protocol.
//
// The path is what the protocol appends to the base URL: the base is
// used as it is written, so the path is all a client adds.
type Protocol struct {
	// Name is the name the protocol is registered under.
	Name string
	// Path is what the protocol appends to the base URL.
	Path string
}

// chatProtocol is a registered chat protocol.
type chatProtocol struct {
	factory Factory
	path    string
}

// decisionProtocol is a registered decision protocol.
type decisionProtocol struct {
	factory DeciderFactory
	path    string
}

var (
	registryMu sync.RWMutex
	protocols  = map[string]chatProtocol{}
	deciders   = map[string]decisionProtocol{}
)

// RegisterChatProtocol adds a chat protocol under name. The path is
// what the protocol appends to the base URL.
//
// A second registration of the same name replaces the first, so a test
// can install a fake.
//
// The concrete protocols register themselves from their package's init.
func RegisterChatProtocol(name, path string, f Factory) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || f == nil {
		return
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	protocols[name] = chatProtocol{factory: f, path: path}
}

// RegisterDecisionProtocol adds a decision protocol under name. The
// path is what the protocol appends to the base URL.
//
// A second registration of the same name replaces the first.
func RegisterDecisionProtocol(name, path string, f DeciderFactory) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || f == nil {
		return
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	deciders[name] = decisionProtocol{factory: f, path: path}
}

// New builds the chat provider the configuration names.
//
// The configuration is validated before the provider is built: an
// unknown protocol, a missing model or a named environment variable
// that holds no key is an error the caller can report. A
// configuration that names no credential at all asks for no
// authentication and is not an error.
func New(cfg ChatConfig) (Provider, error) {
	name := strings.ToLower(strings.TrimSpace(cfg.Protocol))
	if name == "" {
		return nil, fmt.Errorf("llm: no chat protocol configured")
	}
	registryMu.RLock()
	p, ok := protocols[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("llm: unknown chat protocol %q (known: %s)", name, protocolNames(ChatProtocols()))
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("llm: chat protocol %q has no model", name)
	}
	return p.factory(cfg)
}

// NewDecider builds the decider the configuration names.
func NewDecider(cfg DeciderConfig) (Decider, error) {
	name := strings.ToLower(strings.TrimSpace(cfg.Protocol))
	if name == "" {
		return nil, fmt.Errorf("llm: no decision protocol configured")
	}
	registryMu.RLock()
	p, ok := deciders[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("llm: unknown decision protocol %q (known: %s)", name, protocolNames(DecisionProtocols()))
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("llm: decision protocol %q has no model", name)
	}
	return p.factory(cfg)
}

// ChatProtocols lists the registered chat protocols, sorted by name.
func ChatProtocols() []Protocol {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Protocol, 0, len(protocols))
	for name, p := range protocols {
		out = append(out, Protocol{Name: name, Path: p.path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// DecisionProtocols lists the registered decision protocols, sorted by
// name.
func DecisionProtocols() []Protocol {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Protocol, 0, len(deciders))
	for name, p := range deciders {
		out = append(out, Protocol{Name: name, Path: p.path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// protocolNames renders the names of the protocols for an error
// message.
func protocolNames(protocols []Protocol) string {
	names := make([]string, 0, len(protocols))
	for _, p := range protocols {
		names = append(names, p.Name)
	}
	return strings.Join(names, ", ")
}
