// Package codepage decodes the output of Windows console programs.
//
// A Windows console program writes its messages in the console's
// code page, not in UTF-8. On a zh-CN host that code page is 936
// (GBK), so cmd.exe's "the command line is too long" reaches us as
// GBK bytes — a byte sequence that is not valid UTF-8. Every layer
// that marshals such a string to JSON replaces the undecodable
// bytes with U+FFFD, which is what turns a readable diagnostic into
// mojibake on the way to the chat.
//
// The mapping and the decoding live here so the bridge path and the
// shell path share one rule instead of two hand-rolled copies.
package codepage

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// ChineseACP is the Windows ANSI code page for Simplified Chinese
// (GBK / CP936).
const ChineseACP = 936

// Decoder returns the decoder for a Windows ANSI code page, or nil
// when the code page has no decoder here — nil means "pass the
// bytes through unchanged".
//
// Only CP936 is decoded. Every other code page keeps the raw bytes:
// ASCII (the common case on an English Windows) is already valid
// UTF-8, and guessing at the rest would corrupt more than it fixes.
func Decoder(acp uint32) *encoding.Decoder {
	if acp == ChineseACP {
		return simplifiedchinese.GB18030.NewDecoder()
	}
	return nil
}

// LocalDecoder returns the decoder for the host's ANSI code page, or
// nil when the host has none.
func LocalDecoder() *encoding.Decoder {
	return Decoder(LocalACP())
}

// Decode converts raw child-process output into text.
//
// A line that is already valid UTF-8 is kept byte for byte: node
// writes UTF-8 even on a zh-CN host, and only the shell's own
// messages carry the console code page. Decoding the whole buffer at
// once would mangle the UTF-8 half — "pi: 启动失败" read as GB18030
// comes out as "pi: 鍚姩澶辫触" — so the buffer is decoded line by
// line instead.
//
// Splitting on the newline is safe for GB18030: 0x0A never occurs
// inside a multi-byte sequence.
func Decode(raw []byte, dec *encoding.Decoder) string {
	if len(raw) == 0 {
		return ""
	}
	if utf8.Valid(raw) {
		return string(raw)
	}
	if dec == nil {
		return string(raw)
	}
	var b strings.Builder
	for len(raw) > 0 {
		line := raw
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			line, raw = raw[:i+1], raw[i+1:]
		} else {
			raw = nil
		}
		b.WriteString(decodeLine(line, dec))
	}
	return b.String()
}

// decodeLine decodes one line, keeping the raw bytes when the
// decoder rejects them — a truncated multi-byte character at the
// end of the buffer must not cost the rest of the line.
func decodeLine(line []byte, dec *encoding.Decoder) string {
	if utf8.Valid(line) {
		return string(line)
	}
	out, err := dec.Bytes(line)
	if err != nil {
		return string(line)
	}
	return string(out)
}
