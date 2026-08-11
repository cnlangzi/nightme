// Package gateway — re-exports of wire-protocol message types from
// internal/messages. Kept as type aliases for source-compatibility
// with the rest of the codebase; new code should import
// internal/messages directly. See messages package doc for the
// hub-and-spoke architecture rationale.
package gateway

import "github.com/cnlangzi/nightme/internal/messages"

// Wire types — re-exported so existing call sites
// (gateway.OutboundMessage, gateway.SessionContext, …) keep working
// during the gradual migration.

type (
	InboundMessage      = messages.InboundMessage
	Attachment          = messages.Attachment
	ActionPayload       = messages.ActionPayload
	OutboundMessage     = messages.OutboundMessage
	SessionContext      = messages.SessionContext
	ToolInfo            = messages.ToolInfo
	Card                = messages.Card
	CardChoice          = messages.CardChoice
	CardKind            = messages.CardKind
	MessageStatePayload = messages.MessageStatePayload
	UsageInfo           = messages.UsageInfo
	OutboundKind        = messages.OutboundKind
)

// OutboundKind constants — re-exported under the gateway package
// so existing references (gateway.OutReply, gateway.OutResult, …)
// continue to resolve.
const (
	OutReply               = messages.OutReply
	OutToolStart           = messages.OutToolStart
	OutToolEnd             = messages.OutToolEnd
	OutThinking            = messages.OutThinking
	OutMessageState        = messages.OutMessageState
	OutMessageStateRemoved = messages.OutMessageStateRemoved
	OutCard                = messages.OutCard
	OutResult              = messages.OutResult
	OutInit                = messages.OutInit
	OutCommandReply        = messages.OutCommandReply
	OutTaskCreate          = messages.OutTaskCreate
	OutTaskUpdate          = messages.OutTaskUpdate
	OutCardPatch           = messages.OutCardPatch
)

// CardKind constants.
const (
	CardKindPermission = messages.CardKindPermission
	CardKindDecision   = messages.CardKindDecision
	CardKindPreview    = messages.CardKindPreview
)