package messages

import "testing"

func TestEncodeDecodeQuestionPicks(t *testing.T) {
	in := []QuestionPick{
		{ID: "q1", Selected: []string{"A"}},
		{ID: "q2", Selected: []string{}},
		{ID: "q3", Selected: []string{}, Custom: "typed outside options"},
	}
	s := EncodeQuestionPicks(in)
	got, ok := DecodeQuestionPicks(s)
	if !ok {
		t.Fatalf("decode ok = false; encode = %q", s)
	}
	if len(got) != 3 || got[0].ID != "q1" || len(got[0].Selected) != 1 || got[0].Selected[0] != "A" {
		t.Errorf("got = %+v", got)
	}
	if len(got[1].Selected) != 0 {
		t.Errorf("skip pick selected = %v", got[1].Selected)
	}
	if got[2].Custom != "typed outside options" || len(got[2].Selected) != 0 {
		t.Errorf("custom pick = %+v", got[2])
	}
}

func TestDecodeQuestionPicks_PlainLabel(t *testing.T) {
	if _, ok := DecodeQuestionPicks("仅 REPL 启动(裸 nightme)"); ok {
		t.Fatal("plain label must not decode as batch")
	}
}
