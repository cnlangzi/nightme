package config

import (
	"errors"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/cnlangzi/nightme/internal/yamlfile"
)

// The keys of the me.llm section the writer owns.
const (
	meKey       = "me"
	meLLMKey    = "llm"
	meProtocol  = "protocol"
	meBaseURL   = "base_url"
	meModel     = "model"
	meMaxTokens = "max_tokens"
	meAgent     = "agent"
	meAPIKey    = "api_key"
	meAPIKeyEnv = "api_key_env"
)

// defaultMeMaxTokens is the cap the chat model's answers get when
// the configuration does not name one.
const defaultMeMaxTokens = 40960

// MeLLMValues is the chat-model configuration the writer writes.
//
// Every field is optional: the empty string means "no value" and
// removes the key from the file. MaxTokens is the exception — zero
// removes the key as well, and a positive value is written as the
// integer it is.
type MeLLMValues struct {
	// Protocol names the chat protocol ("chat_completions" or
	// "messages").
	Protocol string
	// BaseURL is the root of the API.
	BaseURL string
	// Model is the model to call.
	Model string
	// MaxTokens caps the length of an answer.
	MaxTokens int
	// Agent names the agent that thinks for the owner. It belongs to
	// the me section, not to the chat model.
	Agent string
	// APIKey holds the key itself. It wins over APIKeyEnv.
	APIKey string
	// APIKeyEnv names the environment variable that holds the key.
	APIKeyEnv string
}

// SetMeLLM writes the chat-model values into the me.llm section of
// the config file at path, and the agent that thinks for the owner
// into the me section. Every other node of the document — comments,
// sections and keys nightme does not model — is preserved.
//
// An empty value removes its key. The two credential channels are
// alternatives: whichever one carries a value is written and the
// other is removed, so exactly one of api_key and api_key_env
// survives a write.
//
// A missing file is created; a malformed file is an error and is left
// untouched.
func SetMeLLM(path string, v MeLLMValues) error {
	if path == "" {
		return errors.New("config: empty path")
	}

	configWriteMu.Lock()
	defer configWriteMu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	root, err := yamlfile.Parse(data, path)
	if err != nil {
		return err
	}
	me, err := yamlfile.MapValueOrCreate(root, meKey)
	if err != nil {
		return err
	}
	llmSection, err := yamlfile.MapValueOrCreate(me, meLLMKey)
	if err != nil {
		return err
	}

	setOrDelete(llmSection, meProtocol, v.Protocol)
	setOrDelete(llmSection, meBaseURL, v.BaseURL)
	setOrDelete(llmSection, meModel, v.Model)
	if v.MaxTokens > 0 {
		yamlfile.SetMapValueInt(llmSection, meMaxTokens, v.MaxTokens)
	} else {
		yamlfile.DeleteMapKey(llmSection, meMaxTokens)
	}
	setOrDelete(me, meAgent, v.Agent)

	switch {
	case v.APIKey != "":
		yamlfile.SetMapValue(llmSection, meAPIKey, v.APIKey)
		yamlfile.DeleteMapKey(llmSection, meAPIKeyEnv)
	case v.APIKeyEnv != "":
		yamlfile.SetMapValue(llmSection, meAPIKeyEnv, v.APIKeyEnv)
		yamlfile.DeleteMapKey(llmSection, meAPIKey)
	default:
		yamlfile.DeleteMapKey(llmSection, meAPIKey)
		yamlfile.DeleteMapKey(llmSection, meAPIKeyEnv)
	}

	out, err := yamlfile.Encode(root)
	if err != nil {
		return err
	}
	return yamlfile.WriteFileAtomic(path, out)
}

// setOrDelete writes value under key, or removes the key when the
// value is empty.
func setOrDelete(m *yaml.Node, key, value string) {
	if value == "" {
		yamlfile.DeleteMapKey(m, key)
		return
	}
	yamlfile.SetMapValue(m, key, value)
}
