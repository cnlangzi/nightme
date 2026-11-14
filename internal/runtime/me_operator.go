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
	"strings"
	"sync"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/gateway/observe"
	"github.com/cnlangzi/nightme/internal/messages"
)

// operator drives a chat on behalf of the personal agent.
type operator struct {
	tracker *observe.Tracker

	mu sync.RWMutex
	// channels are the started channels, in the order they started.
	channels []channel.Channel
}

// newOperator constructs an operator.
func newOperator(tracker *observe.Tracker, chs []channel.Channel) *operator {
	return &operator{
		tracker:  tracker,
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
		return "", err
	}
	// The tracker already knows the message from the inbound side;
	// registering it again is harmless and covers a channel that
	// publishes the message on its own.
	o.tracker.Register(msgID, chatID, text)
	return msgID, nil
}

// Act answers a prompt the chat is waiting for.
func (o *operator) Act(ctx context.Context, chatID string, action messages.ActionPayload) error {
	imp, err := o.impersonatorFor(chatID)
	if err != nil {
		return err
	}
	return imp.Act(ctx, chatID, action)
}

// PostCard publishes the run's overview in the chat.
//
// The chat is checked for the ability to speak first: a channel that
// cannot be driven is the clearer reason to refuse a run.
func (o *operator) PostCard(ctx context.Context, chatID string, spec channel.CardSpec) (string, error) {
	if _, err := o.impersonatorFor(chatID); err != nil {
		return "", err
	}
	pub, err := o.cardPublisherFor(chatID)
	if err != nil {
		return "", err
	}
	return pub.PostCard(ctx, chatID, spec)
}

// UpdateCard replaces the run's overview.
func (o *operator) UpdateCard(ctx context.Context, chatID, cardID string, spec channel.CardSpec) error {
	pub, err := o.cardPublisherFor(chatID)
	if err != nil {
		return err
	}
	return pub.UpdateCard(ctx, chatID, cardID, spec)
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

// cardPublisherFor returns the channel that owns the chat, when it can
// publish a card.
func (o *operator) cardPublisherFor(chatID string) (channel.CardPublisher, error) {
	ch, err := o.channelFor(chatID)
	if err != nil {
		return nil, err
	}
	pub, ok := ch.(channel.CardPublisher)
	if !ok {
		return nil, fmt.Errorf("the %s channel cannot show a run card", ch.Name())
	}
	return pub, nil
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
