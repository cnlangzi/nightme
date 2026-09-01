package slack

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/messages"
)

// apiCall is one recorded call against the fake Slack API. Tests
// assert on the sequence of these rather than on wire bytes.
type apiCall struct {
	Method    string
	ChannelID string
	ThreadTS  string
	TS        string
	Text      string
	Chunks    []slackgo.StreamChunk
	Blocks    []slackgo.Block
	Broadcast bool
	Reaction  string
	Status    string
	// TeamID / UserID are captured only by StartStream so we can
	// assert the recipient info is propagated (docs/channel/slack.md §5.2).
	TeamID string
	UserID string
}

// fakeAPI is the scriptable apiClient used across the package tests.
type fakeAPI struct {
	mu    sync.Mutex
	calls []apiCall

	// nextTS is handed out by StartStream / PostMessage.
	tsSeq int

	// failures maps a method name to an error returned on the next
	// call to it; the entry is consumed on use.
	failures map[string]error
	// persistentFail always fails the named method.
	persistentFail map[string]error

	botUserID string
	teamID    string
	authErr   error

	downloadBody []byte
	downloadErr  error
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		failures:       make(map[string]error),
		persistentFail: make(map[string]error),
		botUserID:      "UBOT",
		teamID:         "T1",
	}
}

func (f *fakeAPI) failOnce(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[method] = err
}

func (f *fakeAPI) failAlways(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.persistentFail[method] = err
}

// take pops a scripted failure for method, if any.
func (f *fakeAPI) take(method string) error {
	if err, ok := f.persistentFail[method]; ok {
		return err
	}
	if err, ok := f.failures[method]; ok {
		delete(f.failures, method)
		return err
	}
	return nil
}

func (f *fakeAPI) record(c apiCall) { f.calls = append(f.calls, c) }

func (f *fakeAPI) snapshot() []apiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]apiCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// methods returns just the call names, which is what most ordering
// assertions care about.
func (f *fakeAPI) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.Method)
	}
	return out
}

func (f *fakeAPI) countOf(method string) int {
	n := 0
	for _, m := range f.methods() {
		if m == method {
			n++
		}
	}
	return n
}

func (f *fakeAPI) StartStream(_ context.Context, channelID, threadTS, teamID, userID string, chunks []slackgo.StreamChunk) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("StartStream"); err != nil {
		f.record(apiCall{Method: "StartStream", ChannelID: channelID, ThreadTS: threadTS, Chunks: chunks, TeamID: teamID, UserID: userID})
		return "", err
	}
	f.tsSeq++
	ts := "ts-" + itoa(f.tsSeq)
	f.record(apiCall{Method: "StartStream", ChannelID: channelID, ThreadTS: threadTS, TS: ts, Chunks: chunks, TeamID: teamID, UserID: userID})
	return ts, nil
}

func (f *fakeAPI) AppendStream(_ context.Context, channelID, ts string, chunks []slackgo.StreamChunk) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.take("AppendStream")
	f.record(apiCall{Method: "AppendStream", ChannelID: channelID, TS: ts, Chunks: chunks})
	return err
}

func (f *fakeAPI) StopStream(_ context.Context, channelID, ts string, blocks []slackgo.Block) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.take("StopStream")
	f.record(apiCall{Method: "StopStream", ChannelID: channelID, TS: ts, Blocks: blocks})
	return err
}

func (f *fakeAPI) PostMessage(_ context.Context, channelID, threadTS, text string, blocks []slackgo.Block, broadcast bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("PostMessage"); err != nil {
		return "", err
	}
	f.tsSeq++
	ts := "ts-" + itoa(f.tsSeq)
	f.record(apiCall{
		Method: "PostMessage", ChannelID: channelID, ThreadTS: threadTS,
		TS: ts, Text: text, Blocks: blocks, Broadcast: broadcast,
	})
	return ts, nil
}

func (f *fakeAPI) UpdateMessage(_ context.Context, channelID, ts, text string, blocks []slackgo.Block) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.take("UpdateMessage")
	f.record(apiCall{Method: "UpdateMessage", ChannelID: channelID, TS: ts, Text: text, Blocks: blocks})
	return err
}

func (f *fakeAPI) AddReaction(_ context.Context, channelID, ts, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.take("AddReaction")
	f.record(apiCall{Method: "AddReaction", ChannelID: channelID, TS: ts, Reaction: name})
	return err
}

func (f *fakeAPI) RemoveReaction(_ context.Context, channelID, ts, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.take("RemoveReaction")
	f.record(apiCall{Method: "RemoveReaction", ChannelID: channelID, TS: ts, Reaction: name})
	return err
}

func (f *fakeAPI) SetAssistantStatus(_ context.Context, channelID, threadTS, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.take("SetAssistantStatus")
	f.record(apiCall{Method: "SetAssistantStatus", ChannelID: channelID, ThreadTS: threadTS, Status: status})
	return err
}

func (f *fakeAPI) Download(context.Context, string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(apiCall{Method: "Download"})
	if f.downloadErr != nil {
		return nil, f.downloadErr
	}
	if f.downloadBody == nil {
		return []byte("body"), nil
	}
	return f.downloadBody, nil
}

func (f *fakeAPI) AuthTest(context.Context) (string, string, error) {
	if f.authErr != nil {
		return "", "", f.authErr
	}
	return f.botUserID, f.teamID, nil
}

// fakeSocket is a socketRunner whose event channel the test drives.
type fakeSocket struct {
	events chan socketmode.Event
	mu     sync.Mutex
	acks   int
	runErr error
}

func newFakeSocket() *fakeSocket {
	return &fakeSocket{events: make(chan socketmode.Event, 16)}
}

func (s *fakeSocket) RunContext(ctx context.Context) error {
	<-ctx.Done()
	return s.runErr
}

func (s *fakeSocket) Events() <-chan socketmode.Event { return s.events }

func (s *fakeSocket) Ack(socketmode.Request, ...any) {
	s.mu.Lock()
	s.acks++
	s.mu.Unlock()
}

func (s *fakeSocket) ackCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acks
}

// newTestAdapter builds an Adapter wired to fakes, with the throttle
// disabled by default so tests observe every append individually.
// Tests that care about coalescing set a window explicitly via
// withThrottle.
func newTestAdapter(t *testing.T, api *fakeAPI, sock socketRunner) *Adapter {
	t.Helper()
	cfg := config.SlackConfig{
		BotToken: "xoxb-test",
		AppToken: "xapp-test",
	}
	a, err := NewAdapterWithDeps(cfg, api, sock, t.TempDir())
	if err != nil {
		t.Fatalf("NewAdapterWithDeps: %v", err)
	}
	a.throttle = 0
	a.botUserID = api.botUserID
	a.teamID = api.teamID
	return a
}

// withThrottle rebuilds the adapter's throttle window.
func withThrottle(a *Adapter, d time.Duration) *Adapter {
	a.throttle = d
	return a
}

var errBoom = errors.New("boom")

// hbSnapshot builds a heartbeat with a fixed timestamp so rendering
// assertions stay deterministic.
func hbSnapshot(think, tool int) messages.HeartbeatSnapshot {
	return messages.HeartbeatSnapshot{ThinkCount: think, ToolCount: tool}
}

func toolInfo(name, args string) *messages.ToolInfo {
	return &messages.ToolInfo{Name: name, Args: args}
}

// chunkTexts extracts markdown_text payloads from a recorded call.
func chunkTexts(chunks []slackgo.StreamChunk) []string {
	var out []string
	for _, c := range chunks {
		if md, ok := c.(slackgo.MarkdownTextChunk); ok {
			out = append(out, md.Text)
		}
	}
	return out
}

// taskChunks extracts task_update payloads from a recorded call.
func taskChunks(chunks []slackgo.StreamChunk) []slackgo.TaskUpdateChunk {
	var out []slackgo.TaskUpdateChunk
	for _, c := range chunks {
		if tc, ok := c.(slackgo.TaskUpdateChunk); ok {
			out = append(out, tc)
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
