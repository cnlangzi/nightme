package messages

import (
	"encoding/json"
	"strings"
)

// QuestionBatchPrefix marks a SendPermission payload that carries
// a full AskUserQuestion answer batch (one pick per question id).
// Feishu wizard cards emit this only after the last step; a plain
// option label (no prefix) remains the one-shot / single-question
// path.
const QuestionBatchPrefix = "nm-q:"

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
// payload. ok is false for plain option labels.
func DecodeQuestionPicks(s string) ([]QuestionPick, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, QuestionBatchPrefix) {
		return nil, false
	}
	raw := strings.TrimPrefix(s, QuestionBatchPrefix)
	var picks []QuestionPick
	if err := json.Unmarshal([]byte(raw), &picks); err != nil {
		return nil, false
	}
	return picks, true
}
