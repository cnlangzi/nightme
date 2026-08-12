package login

// GreetingBody is one bilingual greeting unit — each element is
// sent as a single Feishu `post` message carrying both `zh_cn` and
// `en_us` blocks. The receiver's Feishu client picks the locale
// tag matching its UI language, so the same payload renders
// correctly for any user regardless of locale.
//
// Why bilingual-per-element instead of per-locale arrays:
//   - Receiver doesn't need to declare a preference; client picks.
//   - Provider doesn't need to guess locale (we used to switch
//     on tenant_brand and missed users whose SDK response did
//     not echo UserInfo).
//   - Both languages are guaranteed to arrive — no "wrong locale"
//     failure mode where CN user sees English by accident.
//   - Each element is still independent: a transport failure on
//     post 1 doesn't poison post 2.
//
// See docs/channel/feishu.md §Greeting Localization (Strategy B)
// for the empirical verification (API response echo is misleading
// but client renders the matching locale block).
type GreetingBody struct {
	Chinese string
	English string
}

// GreetingMessages is the ordered list of greeting units to send.
// Provider.Greet iterates in order, sending one post per element.
type GreetingMessages []GreetingBody

// GreetingMessageEnglish1 / English2 / Chinese1 / Chinese2 are
// the canonical bodies. They MUST stay in sync element-by-element
// across languages — each `_1` pair describes one beat of the
// greeting, each `_2` pair describes the next. Product approved
// all four together; treat them as one source of truth.
//
// Tone notes (English): "this is NightMe" reads as a polite
// announcement — bot introducing itself, not a person introducing
// themselves. The English version is two short beats: identity
// ("Hi, this is NightMe. Your pair programmer.") then value-prop
// ("Set it running. Stay in the loop from your phone, on your
// terms.") — the second line emphasizes user agency (control over
// when to peek) rather than the bot's autonomy.
//
// Tone notes (Chinese): the second line is the README.zh-CN.md
// §开篇 verbatim — "奔赴你的星辰大海，拥有你的自由生活。那些必须
// 死守电脑、避无可避的无奈，让我替你守候" — so the very first
// message the user sees after binding the bot lands on the same
// voice as the README they (hopefully) skimmed before installing.
// Marketing approved all four texts together; treat them as one
// source of truth.
const (
	GreetingMessageEnglish1 = "Hi, this is NightMe 👋. Your pair programmer."
	GreetingMessageEnglish2 = "Set it running. Stay in the loop from your phone, on your terms 🚀."
	GreetingMessageChinese1 = "你好，我是 NightMe 🌙。"
	GreetingMessageChinese2 = "奔赴你的星辰大海，拥有你的自由生活。那些必须死守电脑、避无可避的无奈，让我替你守候 🛡️。"
)

// GreetingTexts returns the canonical bilingual greeting list. The
// CLI orchestrator passes the result to Provider.Greet(ctx,
// messages); the provider sends each element as a Feishu post with
// both `zh_cn` and `en_us` blocks. Order is preserved.
func GreetingTexts() GreetingMessages {
	return GreetingMessages{
		{
			Chinese: GreetingMessageChinese1,
			English: GreetingMessageEnglish1,
		},
		{
			Chinese: GreetingMessageChinese2,
			English: GreetingMessageEnglish2,
		},
	}
}