// me_operator.go — the operator that drives the chat a run works in.
//
// The chat id names the channel that owns it: chat ids are
// channel-namespaced, so the prefix of the id is the way back to the
// adapter.
//
// The operator also holds the tracker: it is the side channel that
// reports what the daemon said about the messages the run sent.

package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway/observe"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
)

// operator drives a chat on behalf of the personal agent.
type operator struct {
	tracker *observe.Tracker
	logger  *slog.Logger

	mu sync.RWMutex
	// channels are the started channels, in the order they started.
	channels []channel.Channel
}

// newOperator constructs an operator.
func newOperator(tracker *observe.Tracker, chs []channel.Channel, logger *slog.Logger) *operator {
	if logger == nil {
		logger = slog.Default()
	}
	return &operator{
		tracker:  tracker,
		logger:   logger,
		channels: append([]channel.Channel(nil), chs...),
	}
}

// Speak says something in the chat.
func (o *operator) Speak(ctx context.Context, chatID, text string, opts channel.SpeakOptions) (string, error) {
	imp, err := o.impersonatorFor(chatID)
	if err != nil {
		return "", err
	}
	msgID, err := imp.Speak(ctx, chatID, text, opts)
	if err != nil {
		// The message may have reached the chat even though the
		// daemon did not take it: the caller is told which it was.
		return msgID, err
	}
	// The tracker already knows the message from the inbound side;
	// registering it again is harmless and covers a channel that
	// publishes the message on its own.
	o.tracker.Register(msgID, chatID, text)
	return msgID, nil
}

// Ask puts a question in the chat.
//
// The question is visible and the operator can answer it, but the
// daemon does not take it as a turn of its own.
func (o *operator) Ask(ctx context.Context, chatID, text string, opts channel.SpeakOptions) (string, error) {
	imp, err := o.impersonatorFor(chatID)
	if err != nil {
		return "", err
	}
	return imp.Ask(ctx, chatID, text, opts)
}

// Act answers a prompt the chat is waiting for.
func (o *operator) Act(ctx context.Context, chatID string, action messages.ActionPayload) error {
	imp, err := o.impersonatorFor(chatID)
	if err != nil {
		return err
	}
	return imp.Act(ctx, chatID, action)
}

// OnChoice installs the handler that takes the answer of an
// interactive choice back into the run.
//
// The chat that owns the run's chat holds the handler: a click on an
// option comes back as a card action, and the handler the run
// installed takes it.
func (o *operator) OnChoice(chatID string, handler func(requestID, option string) bool) func() {
	cs := findChatSession(chatID)
	if cs == nil {
		return func() {}
	}
	return cs.SetChoiceHandler(handler)
}

// Emit publishes an outbound message in the chat the run works in.
//
// The message travels the same wire the coding agent's events travel:
// the chat's emitter when the chat has a session, and the channel
// itself when it has none — a run that was resumed after a restart
// finds no session, and its events still reach the chat.
//
// The chat's own settings gate the message the way they gate the
// agent's: /think off hides the thinking, /tools off hides the tool
// calls. A chat without a session has no settings, so nothing is
// gated.
//
// The id it returns is the id of the chat message the message
// became: the caller keeps writing into that message when it names
// it as the anchor. It is empty when the chat cannot name it.
func (o *operator) Emit(ctx context.Context, chatID string, msg messages.OutboundMessage) (string, error) {
	ch, err := o.channelFor(chatID)
	if err != nil {
		return "", err
	}
	msg.ChatID = chatID

	cs := findChatSession(chatID)
	if cs == nil {
		return ch.Send(ctx, msg)
	}
	env := agentsession.AgentEventEnvelope{ChatID: chatID}
	for _, p := range outbound.DefaultPolicies(chatsession.GitStatusDeps{}, cs, o.logger) {
		if p.Apply(&msg, env) {
			return "", nil
		}
	}
	em := cs.Emitter()
	if em == nil {
		return ch.Send(ctx, msg)
	}
	return em.Send(ctx, msg)
}

// Get returns the tracked execution of a message.
func (o *operator) Get(msgID string) (observe.Execution, bool) {
	return o.tracker.Get(msgID)
}

// Watch returns a channel of snapshots of an execution.
func (o *operator) Watch(msgID string) <-chan observe.Execution {
	return o.tracker.Watch(msgID)
}

// impersonatorFor returns the channel that owns the chat, when it can
// speak for a program.
func (o *operator) impersonatorFor(chatID string) (channel.Impersonator, error) {
	ch, err := o.channelFor(chatID)
	if err != nil {
		return nil, err
	}
	imp, ok := ch.(channel.Impersonator)
	if !ok {
		return nil, fmt.Errorf("the %s channel cannot be driven by the personal agent", ch.Name())
	}
	return imp, nil
}

// channelFor returns the channel that owns the chat.
func (o *operator) channelFor(chatID string) (channel.Channel, error) {
	o.mu.RLock()
	chs := append([]channel.Channel(nil), o.channels...)
	o.mu.RUnlock()

	for _, ch := range chs {
		if ch == nil {
			continue
		}
		prefix := channel.ChatIDPrefix(ch.Name())
		if prefix == "" || !strings.HasPrefix(chatID, prefix) {
			continue
		}
		return ch, nil
	}
	return nil, fmt.Errorf("no channel owns the chat %q", chatID)
}

// setChannels replaces the channels the operator drives.
//
// The channels are known only after they start: a channel that failed
// to start cannot be driven.
func (o *operator) setChannels(chs []channel.Channel) {
	o.mu.Lock()
	o.channels = append([]channel.Channel(nil), chs...)
	o.mu.Unlock()
}
