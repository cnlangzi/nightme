// Package slack — Slack Socket Mode channel adapter.
//
// This file is the ONLY place in the package that touches
// github.com/slack-go/slack directly. Everything else depends on the
// narrow apiClient / socketRunner interfaces defined here, so tests
// can drive the adapter without a live workspace (mirrors the
// telegram package's apiClient split and feishu's sendFunc fields —
// see docs/channel/slack.md §7.5).
package slack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// apiClient is the narrow Slack Web API surface the adapter needs.
//
// Streaming (StartStream / AppendStream / StopStream) is the primary
// placeholder path; PostMessage / UpdateMessage serve the kinds that
// must bypass the stream (docs/channel/slack.md §4.2).
type apiClient interface {
	// StartStream opens a streaming message and returns its ts.
	// threadTS may be empty for a top-level message.
	StartStream(ctx context.Context, channelID, threadTS string, chunks []slackgo.StreamChunk) (string, error)
	// AppendStream appends chunks to an open stream.
	AppendStream(ctx context.Context, channelID, ts string, chunks []slackgo.StreamChunk) error
	// StopStream finalizes a stream. blocks render below the streamed
	// content (up to 50) and carry the StatusBar footer.
	StopStream(ctx context.Context, channelID, ts string, blocks []slackgo.Block) error

	// PostMessage sends a standalone message. threadTS may be empty.
	// broadcast maps to reply_broadcast (only meaningful with threadTS).
	PostMessage(ctx context.Context, channelID, threadTS, text string, blocks []slackgo.Block, broadcast bool) (string, error)
	// UpdateMessage edits a previously posted message in place.
	UpdateMessage(ctx context.Context, channelID, ts, text string, blocks []slackgo.Block) error

	AddReaction(ctx context.Context, channelID, ts, name string) error
	RemoveReaction(ctx context.Context, channelID, ts, name string) error

	// SetAssistantStatus drives the "is thinking…" indicator. An
	// empty status clears it. Requires the app to have AI features
	// enabled (docs/channel/slack.md §2.5).
	SetAssistantStatus(ctx context.Context, channelID, threadTS, status string) error

	// Download fetches a file behind url_private using the bot token.
	Download(ctx context.Context, urlPrivate string) ([]byte, error)

	// AuthTest returns (botUserID, teamID) for mention detection and
	// chatID namespacing.
	AuthTest(ctx context.Context) (string, string, error)
}

// socketRunner is the narrow Socket Mode surface. Tests substitute a
// fake that pushes events onto a channel they control.
type socketRunner interface {
	RunContext(ctx context.Context) error
	Events() <-chan socketmode.Event
	Ack(req socketmode.Request, payload ...any)
}

// ErrHTMLResponse is returned by Download when Slack answers with an
// HTML page instead of file bytes. That happens when the bot lacks
// the files:read scope — Slack serves a login page with HTTP 200, so
// without this check a whole HTML document would be handed to the
// agent as if it were the user's image (cc-connect hit this; see
// docs/channel/slack.md §4.3).
var ErrHTMLResponse = errors.New("slack: received HTML instead of file bytes (bot likely missing files:read)")

// liveAPI is the production apiClient backed by slack-go.
type liveAPI struct {
	client   *slackgo.Client
	botToken string
	http     *http.Client
}

func newLiveAPI(botToken, appToken string) *liveAPI {
	return &liveAPI{
		client: slackgo.New(botToken,
			slackgo.OptionAppLevelToken(appToken),
		),
		botToken: botToken,
		http:     &http.Client{Timeout: 45 * time.Second},
	}
}

func (a *liveAPI) StartStream(ctx context.Context, channelID, threadTS string, chunks []slackgo.StreamChunk) (string, error) {
	opts := []slackgo.MsgOption{slackgo.MsgOptionChunks(chunks...)}
	if threadTS != "" {
		opts = append(opts, slackgo.MsgOptionTS(threadTS))
	}
	_, ts, err := a.client.StartStreamContext(ctx, channelID, opts...)
	if err != nil {
		return "", fmt.Errorf("slack: start stream: %w", err)
	}
	return ts, nil
}

func (a *liveAPI) AppendStream(ctx context.Context, channelID, ts string, chunks []slackgo.StreamChunk) error {
	_, _, err := a.client.AppendStreamContext(ctx, channelID, ts, slackgo.MsgOptionChunks(chunks...))
	if err != nil {
		return fmt.Errorf("slack: append stream: %w", err)
	}
	return nil
}

func (a *liveAPI) StopStream(ctx context.Context, channelID, ts string, blocks []slackgo.Block) error {
	opts := []slackgo.MsgOption{}
	if len(blocks) > 0 {
		opts = append(opts, slackgo.MsgOptionBlocks(blocks...))
	}
	_, _, err := a.client.StopStreamContext(ctx, channelID, ts, opts...)
	if err != nil {
		return fmt.Errorf("slack: stop stream: %w", err)
	}
	return nil
}

func (a *liveAPI) PostMessage(ctx context.Context, channelID, threadTS, text string, blocks []slackgo.Block, broadcast bool) (string, error) {
	opts := []slackgo.MsgOption{}
	if len(blocks) > 0 {
		opts = append(opts, slackgo.MsgOptionBlocks(blocks...))
	}
	// text is always set: it doubles as the notification fallback
	// even when blocks carry the visible payload.
	opts = append(opts, slackgo.MsgOptionText(text, false))
	if threadTS != "" {
		opts = append(opts, slackgo.MsgOptionTS(threadTS))
		if broadcast {
			opts = append(opts, slackgo.MsgOptionBroadcast())
		}
	}
	_, ts, err := a.client.PostMessageContext(ctx, channelID, opts...)
	if err != nil {
		return "", fmt.Errorf("slack: post message: %w", err)
	}
	return ts, nil
}

func (a *liveAPI) UpdateMessage(ctx context.Context, channelID, ts, text string, blocks []slackgo.Block) error {
	opts := []slackgo.MsgOption{}
	if len(blocks) > 0 {
		opts = append(opts, slackgo.MsgOptionBlocks(blocks...))
	}
	opts = append(opts, slackgo.MsgOptionText(text, false))
	_, _, _, err := a.client.UpdateMessageContext(ctx, channelID, ts, opts...)
	if err != nil {
		return fmt.Errorf("slack: update message: %w", err)
	}
	return nil
}

func (a *liveAPI) AddReaction(ctx context.Context, channelID, ts, name string) error {
	err := a.client.AddReactionContext(ctx, name, slackgo.ItemRef{Channel: channelID, Timestamp: ts})
	// Re-adding an emoji the bot already placed is a no-op, not a
	// failure — the state machine reaches for it on every re-render.
	if err != nil && strings.Contains(err.Error(), "already_reacted") {
		return nil
	}
	return err
}

func (a *liveAPI) RemoveReaction(ctx context.Context, channelID, ts, name string) error {
	err := a.client.RemoveReactionContext(ctx, name, slackgo.ItemRef{Channel: channelID, Timestamp: ts})
	// Removing an emoji that is not there is the desired end state.
	if err != nil && strings.Contains(err.Error(), "no_reaction") {
		return nil
	}
	return err
}

func (a *liveAPI) SetAssistantStatus(ctx context.Context, channelID, threadTS, status string) error {
	return a.client.SetAssistantThreadsStatusContext(ctx, slackgo.AssistantThreadsSetStatusParameters{
		ChannelID: channelID,
		ThreadTS:  threadTS,
		Status:    status,
	})
}

func (a *liveAPI) Download(ctx context.Context, urlPrivate string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlPrivate, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.botToken)
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("slack: download %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if isHTML(data) {
		return nil, ErrHTMLResponse
	}
	return data, nil
}

func (a *liveAPI) AuthTest(ctx context.Context) (string, string, error) {
	resp, err := a.client.AuthTestContext(ctx)
	if err != nil {
		return "", "", err
	}
	return resp.UserID, resp.TeamID, nil
}

// isHTML reports whether the payload looks like an HTML document.
// Slack serves a login page (HTTP 200, text/html) when the token
// cannot read the file, so the status code alone does not catch it.
func isHTML(data []byte) bool {
	head := strings.ToLower(strings.TrimSpace(string(data[:min(len(data), 64)])))
	return strings.HasPrefix(head, "<!doctype") || strings.HasPrefix(head, "<html")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// liveSocket adapts socketmode.Client to socketRunner.
type liveSocket struct {
	client *socketmode.Client
}

func (s *liveSocket) RunContext(ctx context.Context) error { return s.client.RunContext(ctx) }
func (s *liveSocket) Events() <-chan socketmode.Event      { return s.client.Events }
func (s *liveSocket) Ack(req socketmode.Request, payload ...any) {
	_ = s.client.Ack(req, payload...)
}
