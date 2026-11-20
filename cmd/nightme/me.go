// Package main — nightme me (the personal agent's chat model).
//
// The personal agent's chat model lives in the config file under
// me.llm. `nightme me` is the terminal's way into that block: it
// shows what is configured, asks for the fields, and writes them back
// without disturbing the rest of the file.
//
// The wizard is the whole interface — there is no `nightme me get` and
// no `nightme me set`: reading the configuration is part of the
// walk-through, and the file itself is the record.
//
// The daemon builds its models at start-up, so a new chat model takes
// effect on the next restart. The wizard says so.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/config"
	nmerrors "github.com/cnlangzi/nightme/internal/errors"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/llm/chat" // registers the chat protocols
	"github.com/cnlangzi/nightme/internal/runtime"
)

// placeholderKey stands in for a credential the daemon resolves in
// its own environment. It never leaves the process: the build makes
// no network call.
const placeholderKey = "resolved-by-the-daemon"

func newMeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "me",
		Args:  cobra.NoArgs,
		Short: "Configure the personal agent's chat model",
		Long: "Walk through the chat model of the personal agent (me.llm in\n" +
			"the config file): the protocol, the endpoint, the model, the\n" +
			"cap on an answer, the agent that thinks for the owner and the\n" +
			"credential.\n\n" +
			"The values are written to the config file as they are; the rest\n" +
			"of the file is left alone. The daemon builds its models at\n" +
			"start-up, so the new model takes effect on the next restart.",
		RunE: runMe,
	}
}

func runMe(cmd *cobra.Command, args []string) error {
	path := config.DefaultPath()
	cfg, err := config.Load(path)
	if err != nil {
		return nmerrors.Wrap(nmerrors.CodeConfigError, "read the config", err)
	}
	return meWizard(cfg, path, cmd.InOrStdin(), cmd.OutOrStdout())
}

// meWizard asks for the chat model and writes it to the config file.
//
// path is the file the values are written to; the configuration the
// wizard shows and keeps is the one loaded from it.
func meWizard(cfg *config.Config, path string, in io.Reader, out io.Writer) error {
	if cfg == nil {
		return nmerrors.New(nmerrors.CodeConfigError, "read the config")
	}
	br := bufio.NewReader(in)
	current := cfg.Me.LLM

	// abort ends the walk-through without writing anything.
	abort := func() error {
		fmt.Fprintln(out, "Nothing changed.")
		return nil
	}

	printMeState(out, current, cfg.Me.Agent)

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Each field keeps its current value when the answer is empty; q aborts.")

	protocol, ok := askProtocol(br, out, current.Protocol)
	if !ok {
		return abort()
	}
	baseURL, ok := askBaseURL(br, out, current.BaseURL, protocol)
	if !ok {
		return abort()
	}
	model, ok := askValue(br, out, "model", current.Model, acceptAny)
	if !ok {
		return abort()
	}
	maxTokens, ok := askValue(br, out, "max_tokens", current.MaxTokens, func(answer string) (int, bool) {
		n, err := strconv.Atoi(strings.TrimSpace(answer))
		if err != nil || n < 0 {
			fmt.Fprintln(out, "max_tokens must be a number of tokens (0 or more).")
			return 0, false
		}
		if n == 0 {
			fmt.Fprintln(out, "max_tokens 0 removes the cap from the file — the shipped default applies.")
		}
		return n, true
	})
	if !ok {
		return abort()
	}
	agentName, ok := askValue(br, out, "agent", cfg.Me.Agent, func(answer string) (string, bool) {
		name := strings.TrimSpace(answer)
		if isBuiltinAgent(name) {
			return name, true
		}
		fmt.Fprintf(out, "%q is not a built-in agent — the built-ins are: %s\n",
			name, strings.Join(builtinAgentNames(), ", "))
		return "", false
	})
	if !ok {
		return abort()
	}
	apiKey, apiKeyEnv, aborted := askCredential(br, out, in, current)
	if aborted {
		return abort()
	}

	values := config.MeLLMValues{
		Protocol:  protocol,
		BaseURL:   baseURL,
		Model:     model,
		MaxTokens: maxTokens,
		Agent:     agentName,
		APIKey:    apiKey,
		APIKeyEnv: apiKeyEnv,
	}
	if err := config.SetMeLLM(path, values); err != nil {
		return nmerrors.Wrap(nmerrors.CodeConfigError, "write the chat model", err)
	}

	// The file is the record: the report shows what the daemon will
	// read, defaults included.
	effective, err := config.Load(path)
	if err != nil {
		return nmerrors.Wrap(nmerrors.CodeConfigError, "read the config back", err)
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "✓ wrote me.llm and me.agent to %s\n", path)
	printMeState(out, effective.Me.LLM, effective.Me.Agent)

	buildErr := reportChatModel(out, effective.Me.LLM)
	fmt.Fprintln(out, "⚠ the daemon builds its models at start-up — restart it to pick up the new one:")
	fmt.Fprintln(out, "  nightme restart")
	if buildErr != nil {
		return nmerrors.Wrap(nmerrors.CodeConfigError, "the chat model does not build", buildErr)
	}
	return nil
}

// askProtocol asks which wire protocol the chat model speaks.
//
// The answer is the number of a registered protocol or its name. An
// empty answer keeps the current one; a configuration that names none
// has to name one — a chat model without a protocol is no chat model
// at all.
func askProtocol(br *bufio.Reader, out io.Writer, current string) (string, bool) {
	names := llm.ChatProtocolNames()
	for {
		fmt.Fprintln(out, "Protocol:")
		for i, name := range names {
			fmt.Fprintf(out, "  [%d] %s — POST {base_url}%s\n", i+1, name, chat.Path(name))
		}
		fmt.Fprint(out, "> ")
		answer, ok := readAnswer(br)
		if !ok {
			fmt.Fprintln(out)
			return "", false
		}
		if strings.EqualFold(answer, "q") {
			return "", false
		}
		if answer == "" {
			if current != "" {
				return current, true
			}
			fmt.Fprintln(out, "A protocol is required.")
			continue
		}
		if name, ok := matchProtocol(answer, names); ok {
			return name, true
		}
		fmt.Fprintf(out, "%q is not a protocol — choose a number between 1 and %d, or the name of one.\n", answer, len(names))
	}
}

// matchProtocol returns the protocol the answer names: a number in the
// list, or the name of one of them.
func matchProtocol(answer string, names []string) (string, bool) {
	answer = strings.TrimSpace(answer)
	if n, err := strconv.Atoi(answer); err == nil {
		if n < 1 || n > len(names) {
			return "", false
		}
		return names[n-1], true
	}
	name := strings.ToLower(answer)
	for _, known := range names {
		if known == name {
			return known, true
		}
	}
	return "", false
}

// askBaseURL asks for the root of the API.
//
// The protocol decides what is appended to it, so the wizard says
// which path that is: the address the operator writes is the address
// the request goes to, plus that path.
func askBaseURL(br *bufio.Reader, out io.Writer, current, protocol string) (string, bool) {
	if path := chat.Path(protocol); path != "" {
		fmt.Fprintf(out, "The %s protocol calls {base_url}%s\n", protocol, path)
	}
	return askValue(br, out, "base_url", current, acceptAny)
}

// askValue asks for one field and returns the value to write.
//
// An empty answer keeps the current value and "q" aborts the
// walk-through. An exhausted input aborts it too: a wizard with no
// answers left must not write a configuration built from blanks.
//
// parse turns an answer into the field's value and reports whether
// the answer is acceptable; an answer it rejects is asked again.
func askValue[T any](br *bufio.Reader, out io.Writer, name string, current T, parse func(string) (T, bool)) (T, bool) {
	var zero T
	if parse == nil {
		// Every field has a parser; one without has no answer to
		// give, and the walk-through stops rather than writing a
		// guess.
		return zero, false
	}
	for {
		fmt.Fprintf(out, "%s [%v]: ", name, current)
		answer, ok := readAnswer(br)
		if !ok {
			fmt.Fprintln(out)
			return zero, false
		}
		if strings.EqualFold(answer, "q") {
			return zero, false
		}
		if answer == "" {
			return current, true
		}
		value, accepted := parse(answer)
		if !accepted {
			continue
		}
		return value, true
	}
}

// acceptAny takes the answer as it is.
func acceptAny(answer string) (string, bool) { return answer, true }

// askCredential asks which credential the chat model uses.
//
// The four choices are: keep the credential the file has, put the key
// itself in the file, name an environment variable that holds it, or
// drop the credential. The two channels are alternatives — the key
// itself wins over the environment variable — so keeping the
// credential keeps the one that is in force.
func askCredential(br *bufio.Reader, out io.Writer, in io.Reader, current config.MeLLMConfig) (apiKey, apiKeyEnv string, aborted bool) {
	for {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Credential:")
		fmt.Fprintln(out, "  [1] keep the current credential")
		fmt.Fprintln(out, "  [2] set api_key — the key itself")
		fmt.Fprintln(out, "  [3] set api_key_env — the name of an environment variable")
		fmt.Fprintln(out, "  [4] remove the credential")
		fmt.Fprint(out, "> ")
		answer, ok := readAnswer(br)
		if !ok {
			fmt.Fprintln(out)
			return "", "", true
		}
		switch answer {
		case "1":
			if current.APIKey != "" {
				return current.APIKey, "", false
			}
			return "", current.APIKeyEnv, false
		case "2":
			fmt.Fprint(out, "api_key (the key itself): ")
			secret, ok := readSecret(in, br, out)
			if !ok {
				return "", "", true
			}
			if secret == "" {
				fmt.Fprintln(out, "The key cannot be empty.")
				continue
			}
			return secret, "", false
		case "3":
			name, ok := askValue(br, out, "api_key_env", "", acceptAny)
			if !ok {
				return "", "", true
			}
			if name == "" {
				fmt.Fprintln(out, "The variable name cannot be empty.")
				continue
			}
			return "", name, false
		case "4":
			return "", "", false
		case "q":
			return "", "", true
		default:
			fmt.Fprintln(out, "Choose 1, 2, 3, 4 or q.")
		}
	}
}

// readSecret reads one line without echoing it.
//
// The key is read from the terminal itself when the input is a
// terminal and the buffered reader holds no pending bytes — a pending
// byte would be swallowed by the password read. Otherwise the line
// comes from the buffered reader, which is what a piped input gets.
func readSecret(in io.Reader, br *bufio.Reader, out io.Writer) (string, bool) {
	f, ok := in.(*os.File)
	if ok && canReadPassword(f, br) {
		secret, err := term.ReadPassword(int(f.Fd()))
		if err != nil {
			return "", false
		}
		// The terminal swallowed the newline the user typed.
		fmt.Fprintln(out)
		return strings.TrimSpace(string(secret)), true
	}
	return readAnswer(br)
}

// canReadPassword reports whether the key can be read without echo:
// the input has to be the terminal itself, and the buffered reader
// must not be holding bytes — the password read bypasses the reader
// and would swallow them.
func canReadPassword(f *os.File, br *bufio.Reader) bool {
	return isatty.IsTerminal(f.Fd()) && br.Buffered() == 0
}

// readAnswer reads one line from br.
//
// It reports false when the input is exhausted — there is no answer
// to give and nothing to write.
func readAnswer(br *bufio.Reader) (string, bool) {
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return "", false
	}
	return strings.TrimSpace(line), true
}

// reportChatModel builds the chat model the configuration describes
// and reports what it found.
//
// The build makes no network call: a model that builds is a
// well-formed configuration, not an endpoint that answers.
func reportChatModel(out io.Writer, c config.MeLLMConfig) error {
	llmCfg := runtime.ChatModelConfig(c)
	credentialPending := false
	if _, err := llm.APIKey(llmCfg); err != nil {
		// The variable belongs to the daemon's environment, not to
		// the shell the wizard runs in: a variable that is missing
		// here is a warning, not a broken configuration.
		fmt.Fprintf(out, "⚠ %s is not set in this shell — the daemon needs it in its own environment\n",
			llmCfg.APIKeyEnv)
		credentialPending = true
		llmCfg.APIKey = placeholderKey
		llmCfg.APIKeyEnv = ""
	}

	_, err := llm.New(llmCfg)
	if err != nil {
		fmt.Fprintf(out, "chat model: protocol=%s model=%s — does not build: %v\n",
			c.Protocol, c.Model, err)
		return err
	}
	if credentialPending {
		fmt.Fprintf(out, "chat model: protocol=%s model=%s — builds (the credential is the daemon's to resolve)\n",
			c.Protocol, c.Model)
		return nil
	}
	fmt.Fprintf(out, "chat model: protocol=%s model=%s — builds ✓\n", c.Protocol, c.Model)
	return nil
}

// printMeState renders the chat model and the agent that thinks for
// the owner as they are configured now.
func printMeState(out io.Writer, c config.MeLLMConfig, agent string) {
	fmt.Fprintln(out, "Current chat model (me.llm):")
	printMeValues(out, c)
	fmt.Fprintln(out, "Current agent that thinks for the owner (me.agent):")
	fmt.Fprintf(out, "  agent:       %s\n", orNone(agent))
}

// printMeValues renders the fields of the chat model. The credential
// is masked — the wizard never prints a key in full.
func printMeValues(out io.Writer, c config.MeLLMConfig) {
	fmt.Fprintf(out, "  protocol:    %s\n", orNone(c.Protocol))
	fmt.Fprintf(out, "  base_url:    %s\n", orNone(c.BaseURL))
	fmt.Fprintf(out, "  model:       %s\n", orNone(c.Model))
	fmt.Fprintf(out, "  max_tokens:  %d\n", c.MaxTokens)
	fmt.Fprintf(out, "  api_key:     %s\n", maskKey(c.APIKey))
	fmt.Fprintf(out, "  api_key_env: %s\n", orNone(c.APIKeyEnv))
}

// builtinAgentNames lists the agents the registry knows, in
// registration order.
func builtinAgentNames() []string {
	list := agent.Builtins.List()
	names := make([]string, 0, len(list))
	for _, s := range list {
		names = append(names, s.Info().Name)
	}
	return names
}

// isBuiltinAgent reports whether name is one of the registered
// built-in agents.
func isBuiltinAgent(name string) bool {
	_, err := agent.Builtins.Get(name)
	return err == nil
}

// maskKey renders a credential for display: at most the last four
// characters, behind an ellipsis. A key that is not longer than that
// shows none of itself.
func maskKey(key string) string {
	switch {
	case key == "":
		return "(unset)"
	case len(key) <= 4:
		return "…"
	default:
		return "…" + key[len(key)-4:]
	}
}

// orNone renders an empty value as "(unset)".
func orNone(value string) string {
	if value == "" {
		return "(unset)"
	}
	return value
}
