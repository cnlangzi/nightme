# F-26: Gateway Hub Architecture

> **Status**: designing (v0.3)
> **Milestone**: M3 (architecture refactor — gateway becomes central hub for all message traffic)
> **Depends on**: F-08 (Channel), F-20 (Gateway command router), F-21 (agent modes), F-25 (input buffer)
> **Used by**: every Channel + every Agent
> **Related docs**: [SPEC.md §1.1](../SPEC.md), [F-08-channel-abstraction.md](./F-08-channel-abstraction.md), [F-20-gateway.md](./F-20-gateway.md)

## 1. Description

Make **Gateway** the central hub for **all** message traffic — both
inbound (user → agent) and outbound (agent → user). Today inbound
flows through Gateway (slash command dispatch + fallback) but
outbound bypasses Gateway entirely (the session pump calls the
Feishu-specific Renderer directly). This couples the agent runtime
to a single channel implementation; adding Slack or a web UI
would require threading display logic through the agent code.

After this refactor:

- **Gateway** owns the ChatID ↔ SessionID map, the inbound buffer
  (per-chat inbound queue when the agent is busy), the agent
  dispatch (session.SendText), the outbound stream (sequence of
  OutboundMessage from agent events), and the delivery semantics
  ("sent to target" — synchronous fire-and-ack, no retry queue).
- **Channel** is a thin adapter: WebSocket/TCP connection, native
  ↔ Gateway format conversion (Feishu event → `InboundMessage`,
  `OutboundMessage` → Feishu API call), and the **display strategy**
  (Feishu collapses the outbound stream into a single rolling-log
  message with FIFO eviction; Slack could use thread replies + emoji;
  Web could use HTML).
- **Agent** emits `agent.AgentEvent` (existing type, unchanged).
  Gateway translates to `OutboundMessage` and hands to Channel.

Adding a new channel becomes a single-file implementation of the
`Channel` interface — no changes to Gateway or agent code.

## 2. The Message Types (Gateway-owned)

```go
// internal/gateway/messages.go

type InboundMessage struct {
    ChatID    string          // abstract chat ID (Channel maps to its native ID)
    UserID    string          // sender (open_id, slack user id, etc.)
    ChatType  ChatType        // p2p | group | thread
    Text      string          // caption only (attachments below)
    Attachments []Attachment  // already downloaded to LocalPath by Channel
    ReplyTo   string          // thread root message id (for thread-mode replies)
    Raw       any             // channel-private payload (Gateway never inspects)
    Action    *ActionPayload  // for card.action.trigger / button clicks
}

type Attachment struct {
    LocalPath string         // path on local filesystem
    MimeType  string
    Size      int64
    Name      string          // original filename
}

type OutboundMessage struct {
    ChatID  string
    Kind    OutboundKind
    Text    string         // for OutText / OutToolStart / OutToolEnd / OutThinking
    Card    *Card          // for OutCard (interactive permission cards)
    Reaction *Reaction      // for OutReaction / OutReactionRemoved
    ReplyTo string         // thread reply target (for thread-mode replies)
    Meta    map[string]any // msg id, session id, request_id, etc.
}

type OutboundKind int
const (
    OutText OutboundKind = iota
    OutToolStart
    OutToolEnd
    OutThinking
    OutReaction
    OutReactionRemoved
    OutCard
    OutTyping
    // future: OutFile, OutVoice, OutVideo, …
)
```

## 3. The Channel Interface (thin)

```go
// internal/channel/channel.go

type Channel interface {
    Name() string
    Start(ctx context.Context) error
    Stop(ctx context.Context) error

    // Inbound: the channel normalises its native events into
    // InboundMessage and feeds them here. Gateway reads this.
    Incoming() <-chan gateway.InboundMessage

    // Outbound: Gateway hands each OutboundMessage here. Channel
    // formats and sends. "Delivered" means Send() returned nil.
    // Channel may decline to render some kinds (e.g., Slack can't
    // swap reactions in place) — Gateway doesn't care, Channel
    // substitutes or drops.
    Send(ctx context.Context, msg gateway.OutboundMessage) error
}
```

That's the entire surface. No buffering, no command parsing, no
receipt state, no retry — those live at Gateway.

## 4. The Gateway (central hub)

```go
// internal/gateway/gateway.go

type Gateway struct {
    mu        sync.Mutex
    channels  map[string]Channel              // Channel.Name() -> Channel
    sessions  map[string]*Session             // ChatID -> Session
    buffers   map[string][]InboundMessage     // ChatID -> queued inbound
    routes    []Command                       // registered slash commands
}

func (g *Gateway) Start(ctx context.Context, channels []Channel) error
func (g *Gateway) Stop(ctx context.Context) error

// Inbound dispatch (driven by each Channel's Incoming()):
func (g *Gateway) handleInbound(ctx context.Context, ch Channel, msg InboundMessage) error

// Outbound dispatch (driven by each Session's Events channel):
func (g *Gateway) handleOutbound(ctx context.Context, ev agent.AgentEvent) error

// Slash command routing:
func (g *Gateway) Register(cmd Command)
```

Gateway's main loop (one goroutine per Channel + one per Session):

```
ch.Incoming() ─► handleInbound:
                   ├─ match slash command → handler(ctx, msg, args)
                   ├─ else → enqueue to sessions[chatID].buffer
                              (or flush immediately if agent idle)

session.Events() ─► handleOutbound:
                      ├─ AgentEvent → OutboundMessage translator
                      └─ sessions[chatID].channel.Send(ctx, msg)
```

## 5. The Agent (unchanged interface)

```go
// internal/agent/agent.go — existing, untouched.

type Agent interface {
    Name() string
    Mode() Mode
    Command() string
    Args() []string
    Detect() error
    Start(ctx context.Context, cfg StartConfig) (AgentSession, error)
}

type AgentSession interface {
    Events() <-chan AgentEvent
    SendText(ctx context.Context, text string) error
    SendBlocks(ctx context.Context, blocks []ContentBlock) error
    SendPermission(ctx context.Context, choice string) error
    Close() error
}
```

The Agent still emits `AgentEvent{Kind, Text, ToolStart, ToolEnd,
Permission, Done, Error}`. Gateway owns the
`AgentEvent → OutboundMessage` translator (Stage 2 work).

## 6. Migration Stages

| Stage | What changes | Risk |
|---|---|---|
| 1 | Add `internal/gateway/messages.go`. Update Channel interface. Add skeleton Gateway. Behaviour unchanged. | Low — additive |
| 2 | Move session pump + main loop into Gateway. Gateway owns ChatID↔Session map and the inbound buffer. | Medium — main loop rewire |
| 3 | Move Renderer + Receipt from `internal/channel/feishu/` (Gateway-like code) into `internal/channel/feishu/receipt.go` (display strategy). Feishu's rolling-log becomes a Channel-side concern. | High — core migration |
| 4 | Add `internal/channel/echo` to smoke-test the abstractions without external credentials. | Low — additive |

Each stage is its own commit / PR. Stages 2 and 3 should ship
together (a half-done refactor leaves the bot in an inconsistent
state).

## 7. Behaviour preserved by the refactor

- ✅ Slash commands (`/cwd`, `/run`, `/kill`, `/help`, `/agents`)
- ✅ Inbound fallback to session
- ✅ Feishu rolling-log with FIFO eviction
- ✅ Tool output surfacing (`✅ Read → 47 lines`)
- ✅ Thinking surfacing (`💭 I'll explore…`)
- ✅ Permission cards + Allow/Deny round-trip
- ✅ Bidirectional CLI logs (`received: …` + outbound trace)
- ✅ Registration pattern (`agent.Builtins`, `cmd/nightme/agents.go`)

## 8. Behaviour new in v0.3 (after Stage 4)

- ➕ `internal/channel/echo` — second Channel implementation for CI
  smoke testing. Boots without external credentials.
- ➕ `--channel=feishu|echo` flag on `nightme run` (default `feishu`).
- ➕ `nightme channels` subcommand — lists registered channels with
  capability summary (which OutboundKinds they support).

## 9. Out of Scope (v0.3)

- Retry queue / dead-letter — per Devin, "送达 = sent to target"
- Real second IM (Slack/WhatsApp/Telegram) — Stage 4 ships echo only
- Cross-channel bridge (F-11) — requires Channel multiplexing in
  Gateway; defer to v0.4 once Stage 4 lands
- Web UI / TTY (F-16) — separate effort

## 10. Open Questions

1. **Receipt handle vs stateless stream** — does Gateway need to
   tell Channel "this event continues chat turn X" so the Channel
   can keep its rolling-log edit-in-place? The `ChatID` join key
   is enough for the Feishu Channel; Slack's per-message threading
   may want `ReplyTo`. Stage 3 will pin this down.

2. **Permission card `request_id` mapping** — Feishu's card value
   carries the original `EventPermission` request_id. Gateway
   stores a `pendingPermissions map[string]chan string` keyed by
   request_id. Card click → `InboundMessage.Action.Value` →
   Gateway writes to the channel → Session.SendPermission.

3. **What if a Channel can't render an OutboundMessage kind?**
   Stage 3 contract: `Channel.Send` is allowed to silently drop or
   substitute (e.g., Slack swallows `OutReaction` because it can't
   swap reactions in place; Web renders `OutReaction` as an emoji
   span). Gateway never blocks on Channel response — fire-and-ack.

## 11. Test Strategy

- **Unit**: translator (AgentEvent → OutboundMessage) — table-driven
  for every `OutboundKind` × `agent.AgentEvent` kind.
- **Unit**: gateway state machine — session create / append / evict
  / close with mocked Channels.
- **Integration**: two Channels in the same Gateway — verify
  inbound only goes to its own ChatID's session; verify outbound
  reaches the right Channel.
- **End-to-end**: `nightme run --channel=echo` — drive a session
  with a fake Agent; assert the echo Channel prints the expected
  Inbound / Outbound sequence.
- **Regression**: keep every existing Feishu test passing throughout.

## 12. Rollout Plan

1. Stage 1 lands in `main` (behaviour-preserving, additive).
2. Stage 2 + Stage 3 land in a single PR — the half-state is
   unhealthy, so the migration is atomic from a runtime perspective.
3. Stage 4 lands after Stage 2-3 is stable in `main` for at least
   one deploy cycle (i.e., next time Devin cuts a release tag).
4. v0.3 release tag with all four stages merged.

Branch strategy: `refactor/gateway-hub` for Stage 1; rebased
onto `main` as each stage lands.

---

**Status**: designing — Devin signed off on the architecture at
2026-08-01 14:25 (Asia/Shanghai). Implementation begins after this
doc is reviewed.
