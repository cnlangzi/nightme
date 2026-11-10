package loopback

import (
	"context"
	"fmt"

	"github.com/cnlangzi/nightme/internal/messages"
)

// Client is the program-facing API of the loopback channel.
//
// It sends commands into a chat the way a user would type them, and
// it reports what happened from facts — a phase, an outcome, a
// transcript — instead of from rendered cards.
type Client struct {
	ch *Channel
}

// NewClient returns a Client bound to ch.
func NewClient(ch *Channel) *Client {
	return &Client{ch: ch}
}

// Send pushes text into the chat as if a user had typed it, and
// returns the id of the message it became.
//
// The message carries a mention, so the WatchMode gate never drops
// it. The call returns once the daemon has accepted the message into
// its inbound queue — not once the command has finished; use Await
// for that.
func (c *Client) Send(ctx context.Context, chatID, text string) (string, error) {
	return c.ch.send(ctx, chatID, text)
}

// Do sends text and waits for the command to settle.
//
// A command that never settles ends the wait with a timeout
// snapshot: the outcome says "timeout" and the transcript keeps
// whatever the daemon said before the deadline.
func (c *Client) Do(ctx context.Context, chatID, text string) (Execution, error) {
	msgID, err := c.Send(ctx, chatID, text)
	if err != nil {
		return Execution{}, err
	}
	return c.Await(ctx, msgID)
}

// Await blocks until the execution for msgID settles, or until ctx
// ends.
//
// A settle that arrives before this call is never lost: the
// execution keeps its outcome, and Await returns it right away.
//
// When ctx ends first, the returned snapshot carries
// OutcomeTimeout and the partial transcript, and the error explains
// why the wait ended.
func (c *Client) Await(ctx context.Context, msgID string) (Execution, error) {
	ex, ok := c.ch.lookup(msgID)
	if !ok {
		return Execution{}, fmt.Errorf("loopback: no execution for %q", msgID)
	}
	select {
	case <-ex.settledCh:
		return c.ch.take(ex), nil
	case <-ctx.Done():
		snap := c.ch.snapshotOf(ex)
		if snap.Phase == PhaseSettled {
			// The settle raced the deadline. The outcome the
			// daemon reported wins over the deadline.
			return c.ch.take(ex), nil
		}
		snap.Outcome.Status = messages.OutcomeTimeout
		if snap.Outcome.Summary == "" {
			snap.Outcome.Summary = "await deadline exceeded"
		}
		return snap, fmt.Errorf("loopback: await %s: %w", msgID, ctx.Err())
	}
}

// Get returns a snapshot of the execution for msgID, for a status
// query that does not want to consume it.
//
// The second result is false when no such execution is live — it
// never settled, or it was awaited and dropped already.
func (c *Client) Get(msgID string) (Execution, bool) {
	return c.ch.peek(msgID)
}

// Watch returns a channel of snapshots of the execution for msgID.
//
// A snapshot is delivered whenever the execution moves — a phase
// change, a heartbeat, a new line of transcript, a prompt to answer
// — so a driver can report progress, notice a stall (no beat for a
// while) and see what the run still has to answer. The channel is
// closed when the execution settles.
//
// Snapshots are dropped when the watcher falls behind: the daemon's
// outbound path never waits for a reader.
func (c *Client) Watch(msgID string) <-chan Execution {
	return c.ch.watch(msgID)
}
