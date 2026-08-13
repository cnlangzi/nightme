// Package feishu provides the Feishu (Lark) login.Provider for nightme.
//
// QR rendering is split per-OS via build tags so the Unix and
// Windows implementations evolve independently:
//
//   - qrencode_unix.go   (//go:build !windows)
//     RenderASCII: Unicode half-block ("▀", "▄", " ", "█"). Default
//     path on macOS, Linux, *BSD. Works in any terminal that has a
//     monospace font with the half-block glyphs and a UTF-8 output
//     encoding. One terminal row carries two source-module rows.
//
//   - qrencode_windows.go (//go:build windows)
//     RenderANSI: 24-bit ANSI color half-block. Used on Windows
//     Terminal (Cascadia Code, VT processing). The colors come from
//     background/foreground escape codes, so the half-block glyphs
//     don't need to exist in the font — the result is a clean
//     black-and-white QR.
//     WritePNGToDesktop: 512×512 PNG on the user's Desktop, with
//     an instruction caption band underneath. Fallback for legacy
//     conhost (cmd.exe / Windows PowerShell on Win7–Win10 pre-
//     Cascadia, or any conhost where VT processing is off) and for
//     the 120-second time-based backup that fires after the
//     in-terminal QR is shown.
//
// The provider.go entry point (printQRCode) calls a per-OS helper
// (renderQRPlatform) that picks the best mode for the runtime
// platform.
package feishu

import "github.com/skip2/go-qrcode"

// qrcodeErrorLevel trades QR density (more modules) for scan
// reliability. Medium is the skip2 default and is good enough for
// CLI use — no need to chase Low for a few extra rows of vertical
// space in the ASCII path. (The PNG path is size-independent since
// the user can zoom the image.)
const qrcodeErrorLevel = qrcode.Medium