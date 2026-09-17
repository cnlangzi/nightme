package discord

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// interactionAckBudget bounds the time spent on the ACK POST.
// Discord invalidates the interaction token 3 s after the event
// arrives; we leave 500 ms of slack for the surrounding ctx so the
// ACK has a chance to return a useful error before Discord itself
// rejects the request as expired.
const interactionAckBudget = 2500 * time.Millisecond

// onInteraction is wired into gatewayClient.onInteraction. The
// raw d-payload is what Discord delivered — we unmarshal into
// Interaction and dispatch by Type. The function is panic-safe
// (a malformed interaction must not kill the gateway goroutine).
func (a *Adapter) onInteraction(raw json.RawMessage) {
	defer func() {
		if r := recover(); r != nil && a.logger != nil {
			a.logger.Error("discord: onInteraction panicked", "panic", r)
		}
	}()
	var it Interaction
	if err := json.Unmarshal(raw, &it); err != nil {
		if a.logger != nil {
			a.logger.Warn("discord: decode INTERACTION_CREATE", "err", err.Error())
		}
		return
	}
	switch it.Type {
	case InteractionTypeMessageComponent:
		a.handleComponentClick(it)
	case InteractionTypeModalSubmit:
		a.handleModalSubmit(it)
	case InteractionTypePing:
		// PING is a gateway handshake probe; the AcknowledgeInteraction
		// type=1 path is not used in nightme (Discord never sends a
		// user-initiated PING to a bot), so this is defensive.
	default:
		if a.logger != nil {
			a.logger.Debug("discord: unsupported interaction type", "type", it.Type)
		}
	}
}

// handleComponentClick dispatches MESSAGE_COMPONENT events.
// custom_id is either "c:<short>:<idx>" (option click) or
// "i:<short>" ("Type your answer" button).
//
// Both paths use a fresh context.WithTimeout(context.Background(),
// interactionAckBudget) so a cancelled gateway ctx can't strand
// the ACK — Discord invalidates the token 3 s after delivery,
// regardless of caller state.
func (a *Adapter) handleComponentClick(it Interaction) {
	data := it.Data
	if data == nil {
		return
	}
	customID := strings.TrimSpace(data.CustomID)
	switch {
	case strings.HasPrefix(customID, "c:"):
		a.handleChoiceClick(it, customID)
	case strings.HasPrefix(customID, "i:"):
		a.handleInputClick(it, customID)
	default:
		if a.logger != nil {
			a.logger.Debug("discord: unrecognised custom_id", "custom_id", customID)
		}
	}
}

// handleChoiceClick processes a button click on a choice card.
// The ACK is type=7 (UPDATE_MESSAGE) — Discord keeps the original
// message visible until we PATCH it via REST (OutChoicePatch
// eventually arrives on a different code path). We do NOT
// acknowledge the click with a visible reply here; the OutChoicePatch
// path is what visibly settles the card.
func (a *Adapter) handleChoiceClick(it Interaction, customID string) {
	shortReqID, idx, ok := parseChoiceCustomID(customID)
	if !ok {
		return
	}
	a.acknowledgeUpdateMessage(it)
	state, ok := a.choiceStore.GetByShortID(shortReqID)
	if !ok {
		return
	}
	a.publishActionChoice(state, idx, it)
	a.markSettled(state, optionIDFor(state, idx))
}

// handleInputClick pops a modal in response to the "Type your answer"
// button. The modal envelope (title, label, required, max_length)
// is constant across all choice prompts — Discord's MODAL response
// type=9 must include a Title + CustomID + at least one Component.
//
// After the user submits, Discord delivers a second INTERACTION_CREATE
// with Type=5 (MODAL_SUBMIT); handleModalSubmit picks that up.
func (a *Adapter) handleInputClick(it Interaction, customID string) {
	shortReqID, ok := parseInputCustomID(customID)
	if !ok {
		return
	}
	state, ok := a.choiceStore.GetByShortID(shortReqID)
	if !ok {
		// State evicted (daemon restart, prompt settled long ago).
		// Still ACK so the user's button press doesn't show a
		// "This interaction failed" toast.
		a.acknowledgeUpdateMessage(it)
		return
	}
	a.acknowledgeModal(it, inputCustomID(state))
}

// handleModalSubmit consumes a MODAL_SUBMIT event (Type=5). Walks
// the data.components tree looking for the leaf TextInput whose
// CustomID is "answer", publishes the value as an InboundMessage.Action
// with Option="custom" + Form={"answer":...}, and ACKs with type=7
// to close the modal.
func (a *Adapter) handleModalSubmit(it Interaction) {
	answer := extractModalAnswer(it.Data)
	a.acknowledgeUpdateMessage(it)
	modalCustomID := ""
	if it.Data != nil {
		modalCustomID = it.Data.CustomID
	}
	shortReqID := strings.TrimPrefix(modalCustomID, "i:")
	state, ok := a.choiceStore.GetByShortID(shortReqID)
	if !ok {
		return
	}
	a.publishActionInput(state, answer, it)
	a.markSettled(state, "")
}

// extractModalAnswer walks the modal envelope looking for the leaf
// TextInput whose CustomID is "answer". Returns "" when the field
// is absent (modal opened by a stale state; we still ACK the
// interaction but have nothing to publish).
func extractModalAnswer(data *InteractionData) string {
	if data == nil {
		return ""
	}
	for _, row := range data.Components {
		for _, c := range row.Components {
			if c.CustomID == "answer" {
				return c.Value
			}
		}
	}
	return ""
}

// acknowledgeUpdateMessage POSTs type=7 (UPDATE_MESSAGE) within
// Discord's 3-second budget. The body has no Data — Discord keeps
// the original message visible until the runtime follows up with
// an OutChoicePatch that PATCHes it via REST.
func (a *Adapter) acknowledgeUpdateMessage(it Interaction) {
	ctx, cancel := context.WithTimeout(context.Background(), interactionAckBudget)
	defer cancel()
	if err := a.api.AcknowledgeInteraction(ctx, string(it.ID), it.Token, InteractionResponse{Type: 7}); err != nil {
		if a.logger != nil {
			a.logger.Warn("discord: ack UPDATE_MESSAGE failed",
				"interaction_id", string(it.ID),
				"err", err.Error(),
			)
		}
	}
}

// acknowledgeModal POSTs type=9 (MODAL) with a single TextInput
// inside an ActionRow. The title + label + max_length are the
// canonical "Type your answer" prompt; nightme does not
// customise them per prompt because the chatsession layer that
// consumes the InboundMessage.Action doesn't need per-field hints.
func (a *Adapter) acknowledgeModal(it Interaction, modalCustomID string) {
	ctx, cancel := context.WithTimeout(context.Background(), interactionAckBudget)
	defer cancel()
	body := InteractionResponse{
		Type: 9,
		Data: &InteractionData{
			Modal: &Modal{
				Title:    "Your answer",
				CustomID: modalCustomID,
				Components: []Component{{
					Type: ComponentActionRow,
					Components: []Component{{
						Type:        ComponentTextInput,
						CustomID:    "answer",
						Label:       "Your answer",
						Style:       1, // SHORT
						Required:    true,
						MaxLength:   4000,
						Placeholder: "Type your answer here",
					}},
				}},
			},
		},
	}
	if err := a.api.AcknowledgeInteraction(ctx, string(it.ID), it.Token, body); err != nil {
		if a.logger != nil {
			a.logger.Warn("discord: ack MODAL failed",
				"interaction_id", string(it.ID),
				"err", err.Error(),
			)
		}
	}
}

// buildChoiceComponents renders the V1 ActionRow set for the
// current state of a choice prompt. Up to 5 Buttons per row,
// up to 5 rows total. The first option is Primary; the rest are
// Secondary. "Type your answer" is appended as a final Secondary
// button when the choice has at least one Question.
//
// On a settled state the caller is expected to skip this and
// pass components = []Component{} (clears the buttons in place).
func (a *Adapter) buildChoiceComponents(state *choiceState) []Component {
	if state == nil {
		return nil
	}
	options := currentOptions(state)
	rows := []Component{}
	if len(options) > 0 {
		var row []Component
		for i, opt := range options {
			style := ButtonStyleSecondary
			if i == 0 {
				style = ButtonStylePrimary
			}
			row = append(row, Component{
				Type:     ComponentButton,
				Style:    style,
				Label:    opt.Label,
				CustomID: choiceCustomID(state, i),
			})
			if len(row) == 5 || i == len(options)-1 {
				rows = append(rows, Component{Type: ComponentActionRow, Components: row})
				row = nil
			}
		}
	}
	if len(state.Choice.Questions) > 0 {
		rows = append(rows, Component{
			Type: ComponentActionRow,
			Components: []Component{{
				Type:     ComponentButton,
				Style:    ButtonStyleSecondary,
				Label:    "Type your answer",
				CustomID: inputCustomID(state),
			}},
		})
	}
	return rows
}

// renderChoiceContent is the textual body of a choice card. Lives
// in `content` (not in a Button label) so users see the prompt
// without having to expand a component row. Mirrors telegram's
// renderChoice shape minus the HTML markup Discord doesn't need.
//
// Title defaults: "Waiting for approval" for ChoiceKindPermission,
// "Action Needed" otherwise. Body is the prompt body; for AskUser
// questions we render the current step's question instead.
func renderChoiceContent(state *choiceState) string {
	if state == nil || state.Choice == nil {
		return ""
	}
	choice := state.Choice
	title := choice.Title
	if title == "" {
		title = "Action Needed"
	}
	if choice.Kind == messages.ChoiceKindPermission {
		if title == "Action Needed" {
			title = "Waiting for approval"
		}
	} else if title == "Waiting for approval" {
		title = "Action Needed"
	}
	body := ""
	if len(choice.Questions) > 0 {
		if state.Step < len(choice.Questions) {
			body = choice.Questions[state.Step].Question
		}
	} else if choice.Body != "" {
		body = choice.Body
	}
	if body == "" {
		return "**" + title + "**"
	}
	return "**" + title + "**\n\n" + body
}

// currentOptions returns the option list the user is choosing from
// at the current step — AskUserQuestion's per-step slice for
// multi-step choices, or the top-level Options for permissions /
// gtw decisions. Used by both sendChoice (initial render) and
// patchChoice (after step advance).
func currentOptions(state *choiceState) []messages.ChoiceOption {
	if state == nil || state.Choice == nil {
		return nil
	}
	if len(state.Choice.Questions) > 0 {
		if state.Step >= 0 && state.Step < len(state.Choice.Questions) {
			return state.Choice.Questions[state.Step].Options
		}
		return nil
	}
	return state.Choice.Options
}

// optionIDFor returns the ChoiceOption.ID for the given index on
// the current step's option list. Mirrors telegram's per-step
// resolution in callback.go:handleChoiceClick.
func optionIDFor(state *choiceState, optionIndex int) string {
	if state == nil {
		return ""
	}
	options := currentOptions(state)
	if optionIndex < 0 || optionIndex >= len(options) {
		return ""
	}
	return options[optionIndex].ID
}

// parseChoiceCustomID splits "c:<short>:<idx>" into its parts.
// Returns false when the suffix isn't a non-negative integer or
// any part is empty.
func parseChoiceCustomID(customID string) (string, int, bool) {
	parts := strings.Split(customID, ":")
	if len(parts) != 3 || parts[0] != "c" {
		return "", 0, false
	}
	shortReqID := parts[1]
	if shortReqID == "" {
		return "", 0, false
	}
	idx, ok := parseNonNegativeInt(parts[2])
	if !ok {
		return "", 0, false
	}
	return shortReqID, idx, true
}

// parseInputCustomID splits "i:<short>".
func parseInputCustomID(customID string) (string, bool) {
	parts := strings.SplitN(customID, ":", 2)
	if len(parts) != 2 || parts[0] != "i" {
		return "", false
	}
	shortReqID := strings.TrimSpace(parts[1])
	if shortReqID == "" {
		return "", false
	}
	return shortReqID, true
}

// parseNonNegativeInt returns the integer value of s; ok=false when
// s is empty, has leading sign, or contains non-digit bytes.
func parseNonNegativeInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

// publishActionChoice pushes an InboundMessage.Action whose Option
// is the chosen ChoiceOption.ID. chatID / messageID are the
// session chat id and the choice card's Discord message id (the
// latter surfaces as ReplyTo so the chatsession can address the
// card directly when resolving the click).
func (a *Adapter) publishActionChoice(state *choiceState, optionIndex int, it Interaction) {
	if state == nil {
		return
	}
	options := currentOptions(state)
	if optionIndex < 0 || optionIndex >= len(options) {
		return
	}
	inbound := messages.InboundMessage{
		ChatID:     sessionChatID(string(it.ChannelID)),
		UserID:     string(it.User.ID),
		MessageID:  string(it.Message.ID),
		ReplyTo:    string(it.Message.ID),
		HasMention: true,
		Action: &messages.ActionPayload{
			RequestID: state.RequestID,
			Option:    options[optionIndex].ID,
			Raw:       it,
		},
	}
	a.publish(inbound)
}

// publishActionInput pushes an InboundMessage.Action whose Option
// is "custom" and Form carries {"answer": <user-typed text>}.
// Matches the Telegram ForceReply path so the chatsession
// SendPermission / AskUserQuestion machinery picks the answer up
// unchanged across channels.
func (a *Adapter) publishActionInput(state *choiceState, answer string, it Interaction) {
	if state == nil {
		return
	}
	inbound := messages.InboundMessage{
		ChatID:     sessionChatID(string(it.ChannelID)),
		UserID:     string(it.User.ID),
		MessageID:  string(it.Message.ID),
		ReplyTo:    string(it.Message.ID),
		HasMention: true,
		Action: &messages.ActionPayload{
			RequestID: state.RequestID,
			Option:    "custom",
			Form:      map[string]string{"answer": answer},
			Raw:       it,
		},
	}
	a.publish(inbound)
}

// markSettled flips the local settled flag and persists the
// selected option id (when non-empty). Persists via Put so the
// store reflects the click outcome — a follow-up OutChoicePatch
// with Settled=true then becomes a no-op edit (the buttons are
// already cleared by the patch path).
func (a *Adapter) markSettled(state *choiceState, selectedID string) {
	if state == nil {
		return
	}
	state.Settled = true
	if selectedID != "" {
		state.SelectedID = selectedID
	}
	_ = a.choiceStore.Put(state)
}

// asSlogDiscard is the zero-value logger used by tests that don't
// care about log output. Kept here as a named helper rather than
// inlined so the no-logger contract reads consistently across
// callback.go and the test files.
func asSlogDiscard() *slog.Logger { return slog.Default() }
