package messages

import (
	"encoding/json"
	"fmt"
	"strings"
)

// QuestionBatchPrefix marks a SendPermission payload that carries
// a full AskUserQuestion answer batch (one pick per question id).
// Feishu wizard cards emit this only after the last step; a plain
// option label (no prefix) remains the one-shot / single-question
// path.
const QuestionBatchPrefix = "nm-q:"

// QuestionCustomPrefix marks a Choice.Picks slot that came from
// "Type your answer" rather than an option button. Stripped when
// encoding a QuestionBatchPrefix payload.
const QuestionCustomPrefix = "nm-c:"

// QuestionPick is one answered item in a QuestionBatchPrefix payload.
// Skip: empty Selected, empty Custom.
// Option: Selected holds the clicked label.
// Custom ("Type your answer"): Custom is the typed text and
// Selected is empty — host rejects custom combined with selected
// on non-multi questions.
type QuestionPick struct {
	ID       string   `json:"id"`
	Selected []string `json:"selected"`
	Custom   string   `json:"custom,omitempty"`
}

// EncodeQuestionPicks serialises picks as QuestionBatchPrefix+JSON.
func EncodeQuestionPicks(picks []QuestionPick) string {
	b, err := json.Marshal(picks)
	if err != nil {
		return ""
	}
	return QuestionBatchPrefix + string(b)
}

// DecodeQuestionPicks returns the batch when s is a QuestionBatchPrefix
// payload. A plain option label returns (nil, nil). Prefix plus
// invalid JSON is an error — it must not be treated as custom text.
func DecodeQuestionPicks(s string) ([]QuestionPick, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, QuestionBatchPrefix) {
		return nil, nil
	}
	raw := strings.TrimPrefix(s, QuestionBatchPrefix)
	var picks []QuestionPick
	if err := json.Unmarshal([]byte(raw), &picks); err != nil {
		return nil, fmt.Errorf("messages: decode %s payload: %w", QuestionBatchPrefix, err)
	}
	if picks == nil {
		picks = []QuestionPick{}
	}
	return picks, nil
}

// StoreQuestionCustom encodes a typed answer into a Choice.Picks slot.
func StoreQuestionCustom(custom string) string {
	return QuestionCustomPrefix + custom
}

// ParseStoredQuestionPick maps one Choice.Picks slot onto a QuestionPick.
// Empty stored is skip; QuestionCustomPrefix is custom text; anything
// else is a selected option label.
func ParseStoredQuestionPick(id, stored string) QuestionPick {
	p := QuestionPick{ID: id, Selected: []string{}}
	switch {
	case strings.HasPrefix(stored, QuestionCustomPrefix):
		p.Custom = strings.TrimPrefix(stored, QuestionCustomPrefix)
	case stored != "":
		p.Selected = []string{stored}
	}
	return p
}
