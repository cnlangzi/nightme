package login

import (
	"testing"
)

// TestGreetingMessage_ExactCopy guards the user-facing text. These
// strings are the brand contract — Marketing approved all four
// together. If you change one, change its language pair; CI does
// not block it (the strings are just text), but the next reviewer
// will.
func TestGreetingMessage_ExactCopy(t *testing.T) {
	cases := map[string]string{
		"English1": GreetingMessageEnglish1,
		"English2": GreetingMessageEnglish2,
		"Chinese1": GreetingMessageChinese1,
		"Chinese2": GreetingMessageChinese2,
	}
	want := map[string]string{
		"English1": "Hi, this is NightMe 👋. Your pair programmer.",
		"English2": "Set it running. Stay in the loop from your phone, on your terms 🚀.",
		"Chinese1": "你好，我是 NightMe 🌙。",
		"Chinese2": "奔赴你的星辰大海，拥有你的自由生活。那些必须死守电脑、避无可避的无奈，让我替你守候 🛡️。",
	}
	for k, v := range cases {
		if v != want[k] {
			t.Errorf("GreetingMessage%s copy drift\n got: %q\nwant: %q", k, v, want[k])
		}
	}
}

// TestGreetingTexts_BilingualShape asserts the canonical layout:
// 2 elements, each carrying both Chinese and English. The provider
// sends one bilingual post per element; the receiver's Feishu
// client picks the locale tag matching its UI language.
//
// Drift catches:
//   - If a future change adds an _3 element, count goes 2→3 here.
//   - If a future change drops one half (e.g. only Chinese), the
//     pair[0] assertion fails.
//   - If a future change reorders the array, the order assertion
//     in the body asserts catches it.
func TestGreetingTexts_BilingualShape(t *testing.T) {
	body := GreetingTexts()

	if len(body) != 2 {
		t.Fatalf("element count = %d, want 2", len(body))
	}
	if body[0].Chinese != GreetingMessageChinese1 {
		t.Errorf("[0].Chinese = %q, want %q", body[0].Chinese, GreetingMessageChinese1)
	}
	if body[0].English != GreetingMessageEnglish1 {
		t.Errorf("[0].English = %q, want %q", body[0].English, GreetingMessageEnglish1)
	}
	if body[1].Chinese != GreetingMessageChinese2 {
		t.Errorf("[1].Chinese = %q, want %q", body[1].Chinese, GreetingMessageChinese2)
	}
	if body[1].English != GreetingMessageEnglish2 {
		t.Errorf("[1].English = %q, want %q", body[1].English, GreetingMessageEnglish2)
	}

	// Both halves of every element must be non-empty — a half-empty
	// element would render as a one-locale bubble for some users,
	// defeating the bilingual contract.
	for i, b := range body {
		if b.Chinese == "" || b.English == "" {
			t.Errorf("element %d has empty half: zh=%q en=%q", i, b.Chinese, b.English)
		}
	}
}
