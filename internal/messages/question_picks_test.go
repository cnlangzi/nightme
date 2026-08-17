package messages

import "testing"

func TestEncodeDecodeQuestionPicks(t *testing.T) {
	in := []QuestionPick{
		{ID: "q1", Selected: []string{"A"}},
		{ID: "q2", Selected: []string{}},
		{ID: "q3", Selected: []string{}, Custom: "typed outside options"},
	}
	s := EncodeQuestionPicks(in)
	got, err := DecodeQuestionPicks(s)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got == nil {
		t.Fatalf("decode nil; encode = %q", s)
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
	got, err := DecodeQuestionPicks("仅 REPL 启动(裸 nightme)")
	if err != nil {
		t.Fatalf("plain label error = %v", err)
	}
	if got != nil {
		t.Fatal("plain label must not decode as batch")
	}
}

func TestDecodeQuestionPicks_Corrupt(t *testing.T) {
	_, err := DecodeQuestionPicks(QuestionBatchPrefix + "{")
	if err == nil {
		t.Fatal("want error for prefix plus invalid JSON")
	}
}

func TestParseStoredQuestionPick(t *testing.T) {
	if p := ParseStoredQuestionPick("q1", ""); len(p.Selected) != 0 || p.Custom != "" {
		t.Errorf("skip = %+v", p)
	}
	if p := ParseStoredQuestionPick("q1", "A"); len(p.Selected) != 1 || p.Selected[0] != "A" {
		t.Errorf("option = %+v", p)
	}
	if p := ParseStoredQuestionPick("q1", StoreQuestionCustom("typed")); p.Custom != "typed" || len(p.Selected) != 0 {
		t.Errorf("custom = %+v", p)
	}
}
