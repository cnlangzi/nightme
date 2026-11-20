// Package chat implements the chat protocols.
//
// Two wire formats are supported:
//
//   - chat_completions — POST {base}/chat/completions, the format
//     OpenAI, OpenRouter, DeepSeek, Qwen, Ollama and most gateways
//     speak;
//   - messages — POST {base}/messages, the Anthropic Messages API.
//
// The base URL is used as it is written: the client appends the path
// of its own protocol and nothing else, so the address the vendor's
// documentation carries is the address the request goes to. The
// implementation stays lenient — a missing usage block, a null
// content next to tool calls and empty tool arguments are all facts
// of the wild, not errors.
package chat

import (
	"errors"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/llm"
)

// defaultTimeout is the deadline of one call when the configuration
// names none.
const defaultTimeout = 10 * time.Minute

// settings are the parts of a chat configuration every protocol
// needs.
type settings struct {
	// baseURL is the root of the API.
	baseURL string
	// model is the model to call.
	model string
	// apiKey is the credential. Empty means no authentication.
	apiKey string
	// maxTok caps the length of an answer.
	maxTok int
	// headers are the extra headers every request carries.
	headers map[string]string
	// client carries the deadline of a call.
	client *http.Client
}

// newSettings reads the parts of the configuration every chat
// protocol needs.
//
// The base URL is required: a client has no endpoint of its own to
// fall back on. The credential comes from the two channels the
// configuration names.
func newSettings(cfg llm.ChatConfig) (settings, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return settings{}, errors.New("llm/chat: base_url is required")
	}
	key, err := llm.APIKey(cfg)
	if err != nil {
		return settings{}, err
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return settings{
		baseURL: base,
		model:   strings.TrimSpace(cfg.Model),
		apiKey:  key,
		maxTok:  cfg.MaxTokens,
		headers: cfg.Headers,
		client:  &http.Client{Timeout: timeout},
	}, nil
}

// requestHeaders returns the headers a request carries: the ones the
// protocol needs, with the ones of the configuration over them.
func (s settings) requestHeaders(own map[string]string) map[string]string {
	out := make(map[string]string, len(own)+len(s.headers))
	maps.Copy(out, own)
	maps.Copy(out, s.headers)
	return out
}

// Path returns the path a protocol appends to the base URL.
//
// The base URL is used as it is written, so the path of the protocol
// is all a client adds. A protocol the package does not know has no
// path.
func Path(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case protocolCompletions:
		return completionsPath
	case protocolMessages:
		return messagesPath
	}
	return ""
}
