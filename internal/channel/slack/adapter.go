package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/statusbar"
)

// channelTypeIM is Slack's channel_type for a 1:1 DM.
const channelTypeIM = "im"

// Adapter is the Slack channel.Channel implementation.
//
// Transport is Socket Mode only: Slack dials out to us over a
// WebSocket, so a self-hosted daemon needs no public URL, no TLS
// certificate and no signing secret. The trade-off, which matters
// only if nightme ever wants app-directory distribution, is that
// Socket Mode apps cannot be listed in the public Slack Marketplace.
type Adapter struct {
	name     string
	api      apiClient
	socket   socketRunner
	state    *stateStore
	incoming chan messages.InboundMessage
	logger   *slog.Logger
	config   config.SlackConfig
	dataDir  string

	limiter *Limiter
	retry   RetryConfig
	dedup   *dedupIndex
	streams *streamIndex
	health  *socketHealth

	throttle time.Duration

	mu        sync.Mutex
	started   bool
	stopped   bool
	cancel    context.CancelFunc
	botUserID string
	teamID    string

	// publishWG tracks in-flight publishInbound calls so Stop can
	// wait for them before closing a.incoming. Without this, a
	// publish mid-select could panic with "send on closed channel".
	publishWG sync.WaitGroup

	// messageStates dedups repeated OutMessageState emissions and
	// remembers which emoji is currently on a user message, so the
	// next transition can remove it. Slack is the only one of the
	// three channels where that removal is possible: Feishu's
	// reaction API is append-only and Telegram allows a single
	// emoji per message.
	muStates      sync.Mutex
	messageStates map[string]agent.MessageState
}

// NewAdapter builds a production Adapter from config.
func NewAdapter(cfg *config.Config) (*Adapter, error) {
	if cfg == nil {
		return nil, errors.New("slack: config is nil")
	}
	botToken := strings.TrimSpace(cfg.Slack.BotToken)
	appToken := strings.TrimSpace(cfg.Slack.AppToken)
	if botToken == "" {
		return nil, errors.New("slack: bot_token is required")
	}
	if appToken == "" {
		return nil, errors.New("slack: app_token is required (Socket Mode)")
	}
	dataDir := cfg.Paths.DataDir
	if dataDir == "" {
		dataDir = os.TempDir()
	}
	state, err := newStateStore(filepath.Join(dataDir, "slack_state.json"))
	if err != nil {
		return nil, fmt.Errorf("slack: load state: %w", err)
	}

	live := newLiveAPI(botToken, appToken)
	sock := socketmode.New(live.client)

	a := newAdapter(cfg.Slack, live, &liveSocket{client: sock}, state, dataDir)
	return a, nil
}

// NewAdapterWithDeps builds an Adapter around injected transports.
// Exported for the package's own tests; production code uses
// NewAdapter.
//
// Unlike NewAdapter (which returns an error on state-store failure
// and aborts startup), this constructor surfaces the error to the
// caller. Silently substituting an empty in-memory state store would
// disable orphan-stream recovery on the next start — a real footgun
// the original implementation buried.
func NewAdapterWithDeps(cfg config.SlackConfig, api apiClient, socket socketRunner, dataDir string) (*Adapter, error) {
	if dataDir == "" {
		dataDir = os.TempDir()
	}
	state, err := newStateStore(filepath.Join(dataDir, "slack_state.json"))
	if err != nil {
		return nil, fmt.Errorf("slack: load state: %w", err)
	}
	return newAdapter(cfg, api, socket, state, dataDir), nil
}

func newAdapter(cfg config.SlackConfig, api apiClient, socket socketRunner, state *stateStore, dataDir string) *Adapter {
	throttle := time.Duration(cfg.StreamThrottleMs) * time.Millisecond
	if throttle <= 0 {
		throttle = 3 * time.Second
	}
	var limiterCfg *LimiterConfig
	if cfg.RateLimit != nil {
		limiterCfg = &LimiterConfig{
			RatePerSec: cfg.RateLimit.RatePerSec,
			Burst:      cfg.RateLimit.Burst,
		}
	}
	return &Adapter{
		name:          "slack",
		api:           api,
		socket:        socket,
		state:         state,
		incoming:      make(chan messages.InboundMessage, 128),
		logger:        slog.Default(),
		config:        cfg,
		dataDir:       dataDir,
		limiter:       NewLimiter(limiterCfg, slog.Default()),
		retry:         DefaultRetryConfig,
		dedup:         newDedupIndex(defaultDedupCap, defaultDedupTTL),
		streams:       newStreamIndex(defaultStreamIndexCap),
		health:        newSocketHealth(),
		throttle:      throttle,
		messageStates: make(map[string]agent.MessageState),
	}
}

func (a *Adapter) Name() string { return a.name }

func (a *Adapter) Incoming() <-chan messages.InboundMessage { return a.incoming }

func (a *Adapter) SetLogger(logger *slog.Logger) {
	if logger == nil {
		return
	}
	a.mu.Lock()
	a.logger = logger
	a.mu.Unlock()
}

func (a *Adapter) log() *slog.Logger {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.logger == nil {
		return slog.Default()
	}
	return a.logger
}

// HealthSnapshot serves the daemoncontrol "health" RPC.
func (a *Adapter) HealthSnapshot() (string, json.RawMessage, error) {
	payload, err := a.health.marshal()
	if err != nil {
		return a.name, nil, err
	}
	return a.name, payload, nil
}

// BuildBlocks converts an inbound message into agent content blocks.
// Slack has no rich inbound envelope to preserve (unlike Feishu's
// post type), so this is text plus whatever files were downloaded.
func (a *Adapter) BuildBlocks(text string, attachments []messages.Attachment) []agent.ContentBlock {
	return BuildBlocks(text, attachments)
}

// BuildBlocks is the package-level helper behind Adapter.BuildBlocks.
func BuildBlocks(text string, attachments []messages.Attachment) []agent.ContentBlock {
	var blocks []agent.ContentBlock
	if text != "" {
		blocks = append(blocks, agent.ContentBlock{Type: agent.ContentText, Text: text})
	}
	for _, att := range attachments {
		if att.LocalPath == "" {
			// The InboundMessage contract requires LocalPath to be
			// filled before publishing; an empty one means the
			// download failed and the bytes do not exist.
			continue
		}
		blockType := agent.ContentFile
		if strings.HasPrefix(att.MimeType, "image/") {
			blockType = agent.ContentImage
		}
		blocks = append(blocks, agent.ContentBlock{
			Type:      blockType,
			Path:      att.LocalPath,
			MediaType: att.MimeType,
		})
	}
	return blocks
}

// Start opens the Socket Mode connection and begins publishing on
// Incoming.
func (a *Adapter) Start(ctx context.Context) error {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return nil
	}
	if a.stopped {
		a.mu.Unlock()
		return errors.New("slack: adapter already stopped")
	}
	a.started = true
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	a.cancel = cancel
	a.mu.Unlock()

	// Identify ourselves before consuming events: mention detection
	// needs the bot user id and every chat id embeds the team id.
	botUserID, teamID, err := a.api.AuthTest(ctx)
	if err != nil {
		cancel()
		a.mu.Lock()
		a.started = false
		a.mu.Unlock()
		// Graceful shutdown looks like an auth failure without this
		// branch — surface the real cause.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("slack: startup cancelled: %w", err)
		}
		return fmt.Errorf("slack: auth test: %w", err)
	}
	a.mu.Lock()
	a.botUserID = botUserID
	a.teamID = teamID
	a.mu.Unlock()

	// Close any stream a previous process left open before we start
	// minting new ones.
	a.recoverOrphanStreams(runCtx)

	go a.eventLoop(runCtx)
	go func() {
		a.health.record(HealthConnecting, "")
		if err := a.socket.RunContext(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			a.health.record(HealthError, err.Error())
			a.log().Error("slack: socket mode stopped", "err", err)
		}
	}()
	return nil
}

// Stop closes the connection, drains open streams and closes
// Incoming.
func (a *Adapter) Stop(ctx context.Context) error {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return nil
	}
	a.stopped = true
	cancel := a.cancel
	a.cancel = nil
	a.mu.Unlock()

	// Close every stream we still own so the user is not left with a
	// message rendered as perpetually in progress.
	for _, s := range a.streams.all() {
		if err := s.finish(ctx); err != nil {
			a.log().Warn("slack: closing stream on shutdown failed",
				"chat_id", s.chatID, "err", err)
		}
	}

	if cancel != nil {
		cancel()
	}
	a.health.record(HealthDisconnected, "adapter stopped")
	// Wait for in-flight publishInbound calls to finish before
	// closing a.incoming — otherwise a goroutine blocked on the
	// select's send case would panic.
	a.publishWG.Wait()
	close(a.incoming)
	return nil
}

// recoverOrphanStreams closes streams a previous process started but
// never stopped (docs/channel/slack.md §3.5).
//
// A persistent close failure must NOT evict the local record — the
// Slack stream itself is still open and the next start should retry.
// Eviction on failure used to leak Slack streams forever whenever the
// close transiently failed (network blip, Slack rate limit, etc.).
func (a *Adapter) recoverOrphanStreams(ctx context.Context) {
	orphans := a.state.orphanStreams(time.Now().UTC())
	if len(orphans) == 0 {
		return
	}
	a.log().Info("slack: closing orphaned streams from previous run", "count", len(orphans))
	for _, rec := range orphans {
		err := withTransientRetry(ctx, a.retry, a.log(), "stopStream/orphan", func() error {
			if err := a.limiter.Wait(ctx); err != nil {
				return err
			}
			return a.api.StopStream(ctx, rec.ChannelID, rec.TS, nil)
		})
		if err != nil {
			a.log().Warn("slack: orphan stream close failed; will retry next start",
				"channel", rec.ChannelID, "ts", rec.TS, "err", err)
			continue
		}
		a.state.dropStream(rec.ChannelID, rec.TS)
	}
}

// eventLoop drains the Socket Mode event channel.
func (a *Adapter) eventLoop(ctx context.Context) {
	events := a.socket.Events()
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			a.handleSocketEvent(ctx, evt)
		}
	}
}

// handleSocketEvent routes one Socket Mode event.
//
// Acks fire before any filtering: an unacked request is one Slack
// will redeliver, and a message we intentionally dropped should not
// come back.
func (a *Adapter) handleSocketEvent(ctx context.Context, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeConnecting:
		a.health.record(HealthConnecting, "")
	case socketmode.EventTypeConnected:
		a.health.record(HealthConnected, "")
	case socketmode.EventTypeConnectionError:
		a.health.record(HealthError, fmt.Sprint(evt.Data))
	case socketmode.EventTypeDisconnect:
		a.health.record(HealthDisconnected, "")

	case socketmode.EventTypeEventsAPI:
		if evt.Request != nil {
			a.socket.Ack(*evt.Request)
		}
		payload, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return
		}
		a.handleEventsAPI(ctx, payload)

	case socketmode.EventTypeSlashCommand:
		if evt.Request != nil {
			a.socket.Ack(*evt.Request)
		}
		cmd, ok := evt.Data.(slackgo.SlashCommand)
		if !ok {
			return
		}
		a.handleSlashCommand(cmd)

	case socketmode.EventTypeInteractive:
		if evt.Request != nil {
			a.socket.Ack(*evt.Request)
		}
		callback, ok := evt.Data.(slackgo.InteractionCallback)
		if !ok {
			return
		}
		a.handleInteractive(callback)
	}
}

// handleEventsAPI dispatches the inner Events API payloads.
func (a *Adapter) handleEventsAPI(ctx context.Context, payload slackevents.EventsAPIEvent) {
	if payload.Type != slackevents.CallbackEvent {
		return
	}
	// Files are pulled from the raw inner event rather than the typed
	// struct: slackevents.MessageEvent / AppMentionEvent do not model
	// a Files field, but Slack does send `files` on both when the
	// user attaches something. cc-connect hit the same gap and
	// re-parses the raw JSON for exactly this reason.
	var rawInner *json.RawMessage
	if cb, ok := payload.Data.(slackevents.EventsAPICallbackEvent); ok {
		rawInner = cb.InnerEvent
	}
	files := parseInnerEventFiles(rawInner)
	parentUserID := parseInnerEventParentUser(rawInner)

	switch inner := payload.InnerEvent.Data.(type) {
	case *slackevents.AppMentionEvent:
		a.handleAppMention(ctx, inner, files)
	case *slackevents.MessageEvent:
		a.handleMessage(ctx, inner, files, parentUserID)
	}
}

// parseInnerEventFiles digs `files` out of the raw inner event.
func parseInnerEventFiles(raw *json.RawMessage) []slackgo.File {
	if raw == nil || len(*raw) == 0 {
		return nil
	}
	var wrapper struct {
		Files []slackgo.File `json:"files"`
	}
	if err := json.Unmarshal(*raw, &wrapper); err != nil {
		return nil
	}
	return wrapper.Files
}

// parseInnerEventParentUser digs `parent_user_id` out of the raw
// inner event — the marker that says this message is a reply inside
// a thread, and to whom.
func parseInnerEventParentUser(raw *json.RawMessage) string {
	if raw == nil || len(*raw) == 0 {
		return ""
	}
	var wrapper struct {
		ParentUserID string `json:"parent_user_id"`
	}
	if err := json.Unmarshal(*raw, &wrapper); err != nil {
		return ""
	}
	return wrapper.ParentUserID
}

// handleAppMention processes an @-mention in a channel.
//
// nightme subscribes to app_mention AND message.channels, so this
// event and a MessageEvent describe the same Slack message. The
// dedup index makes whichever arrives first the one that counts.
func (a *Adapter) handleAppMention(ctx context.Context, ev *slackevents.AppMentionEvent, files []slackgo.File) {
	if ev == nil || ev.BotID != "" || ev.User == "" {
		return
	}
	if a.dedup.seen(ev.Channel, ev.TimeStamp) {
		return
	}
	a.publishInbound(ctx, inboundSource{
		channelID:   ev.Channel,
		channelType: "",
		userID:      ev.User,
		text:        ev.Text,
		ts:          ev.TimeStamp,
		threadTS:    ev.ThreadTimeStamp,
		files:       files,
		hasMention:  true, // an app_mention is by definition addressed to us
	})
}

// handleMessage processes a DM or channel message.
func (a *Adapter) handleMessage(ctx context.Context, ev *slackevents.MessageEvent, files []slackgo.File, parentUserID string) {
	if ev == nil || ev.BotID != "" || ev.User == "" {
		return
	}
	// Edits, deletions, joins and the rest are not user turns.
	if ev.SubType != "" && ev.SubType != "file_share" {
		return
	}
	a.mu.Lock()
	botUserID := a.botUserID
	a.mu.Unlock()
	if ev.User == botUserID {
		return
	}
	if a.dedup.seen(ev.Channel, ev.TimeStamp) {
		return
	}

	a.publishInbound(ctx, inboundSource{
		channelID:    ev.Channel,
		channelType:  ev.ChannelType,
		userID:       ev.User,
		text:         ev.Text,
		ts:           ev.TimeStamp,
		threadTS:     ev.ThreadTimeStamp,
		parentUserID: parentUserID,
		files:        files,
	})
}

// inboundSource is the normalized shape shared by the mention and
// message paths.
type inboundSource struct {
	channelID    string
	channelType  string
	userID       string
	text         string
	ts           string
	threadTS     string
	parentUserID string
	files        []slackgo.File
	hasMention   bool
}

// publishInbound converts a Slack event into an InboundMessage.
//
// The publishWG.Add/Done pair lets Stop wait for in-flight publishes
// to finish before closing a.incoming — otherwise a select blocked on
// `a.incoming <- msg` could panic with "send on closed channel".
func (a *Adapter) publishInbound(ctx context.Context, src inboundSource) {
	// Refuse to start a publish after Stop has flipped the flag; the
	// event is dropped on the floor. publishWG.Add is paired with the
	// matching Done below.
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return
	}
	a.publishWG.Add(1)
	a.mu.Unlock()
	defer a.publishWG.Done()

	botUserID := a.botUserID
	teamID := a.teamID

	chatID := sessionChatID(teamID, src.channelID, src.threadTS)
	if chatID == "" {
		a.log().Warn("slack: dropping message with unresolvable chat id",
			"channel", src.channelID)
		return
	}

	text := stripMentionPrefix(src.text, botUserID)
	hasMention := src.hasMention ||
		computeHasMention(src.channelType, src.text, botUserID, src.parentUserID)

	attachments := a.downloadFiles(ctx, src.files)

	msg := messages.InboundMessage{
		ChatID:      chatID,
		UserID:      src.userID,
		Text:        text,
		MessageID:   src.ts,
		Time:        slackTimestamp(src.ts),
		Attachments: attachments,
		HasMention:  hasMention,
		Raw:         src,
	}
	if len(attachments) > 0 {
		msg.Blocks = BuildBlocks(text, attachments)
	}

	a.health.recordInbound()
	select {
	case a.incoming <- msg:
	case <-ctx.Done():
	}
}

// downloadFiles fetches each attached file into the data dir.
func (a *Adapter) downloadFiles(ctx context.Context, files []slackgo.File) []messages.Attachment {
	if len(files) == 0 {
		return nil
	}
	dir := filepath.Join(a.dataDir, "slack-attachments")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		a.log().Warn("slack: cannot create attachment dir", "err", err)
		return nil
	}
	out := make([]messages.Attachment, 0, len(files))
	for _, f := range files {
		url := f.URLPrivateDownload
		if url == "" {
			url = f.URLPrivate
		}
		att := messages.Attachment{
			MimeType: f.Mimetype,
			Name:     f.Name,
			FileName: f.Name,
			FileKey:  f.ID,
			Type:     attachmentType(f.Mimetype),
		}
		if url == "" {
			att.Error = errors.New("slack: file has no private URL")
			out = append(out, att)
			continue
		}
		data, err := a.api.Download(ctx, url)
		if err != nil {
			att.Error = err
			a.log().Warn("slack: attachment download failed",
				"file", f.Name, "err", err)
			out = append(out, att)
			continue
		}
		name := f.ID
		if f.Name != "" {
			name = f.ID + "-" + filepath.Base(f.Name)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			att.Error = err
			out = append(out, att)
			continue
		}
		att.LocalPath = path
		att.Size = int64(len(data))
		if att.MimeType == "" {
			att.MimeType = "application/octet-stream"
		}
		out = append(out, att)
	}
	return out
}

func attachmentType(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	case strings.HasPrefix(mime, "video/"):
		return "media"
	default:
		return "file"
	}
}

// handleSlashCommand turns a Slack slash command into a normal
// inbound text message so the existing command pipeline handles it.
//
// Two extra steps before pushing to the dispatcher:
//
//  1. Post a "⏳ Processing /<cmd> …" placeholder message and use
//     its ts as the InboundMessage.MessageID. Slack's
//     chat.startStream/chat.appendStream APIs reject streaming when
//     thread_ts is cmd.TriggerID (which is a transient request
//     token, not a message ts), and SlashOutput replies
//     (chat.postMessage with thread_ts) need a real parent ts to
//     render in the user's view. The placeholder message is the
//     thread root for every downstream API call.
//  2. Map /kclose → /close (manifest can't register /close because
//     Slack reserves it; see docs/channel/slack.md §6.2.1).
//
// The placeholder post is best-effort: if it fails (channel not
// joined, rate limit), we fall back to cmd.TriggerID so the command
// still runs, even though subsequent streaming will fail until the
// daemon is restarted with the binary that fixes Issue B.
func (a *Adapter) handleSlashCommand(cmd slackgo.SlashCommand) {
	a.mu.Lock()
	teamID := a.teamID
	a.mu.Unlock()
	if teamID == "" {
		teamID = cmd.TeamID
	}

	chatID := sessionChatID(teamID, cmd.ChannelID, "")
	if chatID == "" {
		return
	}
	command := cmd.Command
	// /close is reserved by Slack (built-in channel close). Manifest
	// registers it as /kclose so the validator accepts it; we
	// translate back to /close so the existing command parser still
	// routes to internal/command/close.
	if command == "/kclose" {
		command = "/close"
	}
	text := command
	if strings.TrimSpace(cmd.Text) != "" {
		text += " " + cmd.Text
	}

	// Post the thread-root placeholder. Use the slackgo client
	// directly so we keep the existing liveAPI.retry / .limiter
	// wrapping on the hot path (post() uses them).
	parentCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	parentTS, err := a.post(parentCtx, cmd.ChannelID, "", "⏳ Processing "+command+" …", nil, false)
	if err != nil || parentTS == "" {
		a.log().Warn("slack: slash command parent post failed; falling back to TriggerID",
			"cmd", command, "err", err)
		parentTS = cmd.TriggerID
	}

	a.health.recordInbound()
	msg := messages.InboundMessage{
		ChatID:     chatID,
		UserID:     cmd.UserID,
		Text:       text,
		MessageID:  parentTS, // real Slack ts from the placeholder post
		Time:       time.Now(),
		HasMention: true, // an explicit command is always for us
		Raw:        cmd,
	}
	select {
	case a.incoming <- msg:
	default:
		a.log().Warn("slack: inbound buffer full, dropping slash command", "cmd", cmd.Command)
	}
}

// handleInteractive turns a Block Kit button press into an
// ActionPayload. Correlation stays Channel-private: callers match on
// Choice.RequestID and never see a Slack message id.
func (a *Adapter) handleInteractive(cb slackgo.InteractionCallback) {
	if cb.Type != slackgo.InteractionTypeBlockActions || len(cb.ActionCallback.BlockActions) == 0 {
		return
	}
	action := cb.ActionCallback.BlockActions[0]
	requestID, optionID, ok := parseActionValue(action.Value)
	if !ok {
		return
	}
	a.mu.Lock()
	teamID := a.teamID
	a.mu.Unlock()
	// Mirror handleSlashCommand: when AuthTest has not yet populated
	// a.teamID (race during early startup, or a token that never
	// authenticated), fall back to the team id on the callback
	// itself so the message is not silently dropped.
	if teamID == "" {
		teamID = cb.Team.ID
	}
	chatID := sessionChatID(teamID, cb.Channel.ID, cb.Message.ThreadTimestamp)
	if chatID == "" {
		return
	}
	a.health.recordInbound()
	msg := messages.InboundMessage{
		ChatID:     chatID,
		UserID:     cb.User.ID,
		MessageID:  cb.Message.Timestamp,
		Time:       time.Now(),
		HasMention: true, // clicking our button is unambiguous intent
		Action: &messages.ActionPayload{
			RequestID: requestID,
			Option:    optionID,
			Raw:       cb,
		},
		Raw: cb,
	}
	select {
	case a.incoming <- msg:
	default:
		a.log().Warn("slack: inbound buffer full, dropping block action")
	}
}

// slackTimestamp parses Slack's "1234567890.123456" message ts.
func slackTimestamp(ts string) time.Time {
	dot := strings.IndexByte(ts, '.')
	secPart := ts
	if dot >= 0 {
		secPart = ts[:dot]
	}
	var sec int64
	if _, err := fmt.Sscanf(secPart, "%d", &sec); err != nil || sec == 0 {
		return time.Now()
	}
	return time.Unix(sec, 0)
}

var _ channel.Channel = (*Adapter)(nil)

// statusBarLines is a thin indirection so tests can assert footer
// wiring without constructing a full OutboundMessage graph.
func statusBarLines(msg *messages.OutboundMessage) []string {
	return statusbar.StatusBarLines(msg)
}
