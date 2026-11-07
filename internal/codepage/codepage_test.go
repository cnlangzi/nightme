package codepage

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// cmdTooLongGBK is the byte sequence cmd.exe writes for "the command
// line is too long" on a zh-CN host (CP936), captured from a
// CREATE_NO_WINDOW spawn. It is the exact payload the Windows
// stderr regression is about.
var cmdTooLongGBK = []byte{
	0xc3, 0xfc, 0xc1, 0xee, 0xd0, 0xd0, 0xcc, 0xab,
	0xb3, 0xa4, 0xa1, 0xa3, 0x0d, 0x0a,
}

func TestDecode_CP936CommandLineTooLong(t *testing.T) {
	if utf8.Valid(cmdTooLongGBK) {
		t.Fatal("the fixture must not be valid UTF-8 — the decode would be a no-op")
	}
	got := Decode(cmdTooLongGBK, Decoder(ChineseACP))
	want := "命令行太长。\r\n"
	if got != want {
		t.Errorf("Decode = %q, want %q", got, want)
	}
}

func TestDecode_PreservesValidUTF8(t *testing.T) {
	in := []byte("pi: ready\n模型已加载\n")
	got := Decode(in, Decoder(ChineseACP))
	if got != string(in) {
		t.Errorf("Decode = %q, want %q", got, string(in))
	}
}

func TestDecode_MixedStreamKeepsBothHalvesReadable(t *testing.T) {
	// node writes UTF-8; cmd.exe writes CP936. Decoding the whole
	// buffer as GB18030 would mangle the UTF-8 half.
	in := append([]byte("pi: 启动失败\n"), cmdTooLongGBK...)
	got := Decode(in, Decoder(ChineseACP))
	if !strings.Contains(got, "pi: 启动失败") {
		t.Errorf("UTF-8 half mangled: %q", got)
	}
	if !strings.Contains(got, "命令行太长。") {
		t.Errorf("CP936 half not decoded: %q", got)
	}
}

func TestDecode_NilDecoderPassesRaw(t *testing.T) {
	got := Decode(cmdTooLongGBK, nil)
	if got != string(cmdTooLongGBK) {
		t.Errorf("Decode with nil decoder = %q, want the raw bytes", got)
	}
}

func TestDecode_EmptyInput(t *testing.T) {
	if got := Decode(nil, Decoder(ChineseACP)); got != "" {
		t.Errorf("Decode(nil) = %q, want empty", got)
	}
}

func TestDecoder_NonChineseCodePagesPassThrough(t *testing.T) {
	for _, acp := range []uint32{0, 1252, 932, 949, 950} {
		if got := Decoder(acp); got != nil {
			t.Errorf("Decoder(%d) = %v, want nil (raw passthrough)", acp, got)
		}
	}
}
