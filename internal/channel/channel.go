// Package channel defines the protocol-neutral boundary between nightme and
// an instant-messaging backend.
package channel

import (
	"context"
	"time"
)

// Message is a normalized incoming message from a channel.
//
// Channel adapters strip protocol-specific markup before publishing a Message;
// Raw payload support can be added without changing the daemon contract.
type Message struct {
	ChatID   string
	Text     string
	SenderID string
	Time     time.Time
}

// Channel is the lifecycle and messaging contract implemented by each IM
// adapter.
type Channel interface {
	// Start starts the adapter's long-lived receive loop.
	Start(ctx context.Context) error

	// Stop closes the receive loop and releases adapter resources.
	Stop(ctx context.Context) error

	// SendMessage sends one text message to chatID.
	SendMessage(ctx context.Context, chatID, text string) error

	// SendLongMessage sends text in channel-safe chunks.
	SendLongMessage(ctx context.Context, chatID, text string) error

	// Incoming returns normalized messages received from the channel.
	Incoming() <-chan Message
}
