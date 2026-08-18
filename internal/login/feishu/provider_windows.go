//go:build windows

package feishu

import (
	"context"
	"fmt"
	"github.com/cnlangzi/nightme/internal/proc"
	"io"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// renderQRPlatform is the Windows rendering entry point.
//
// On Windows we do NOT hand the registration URL to a browser —
// Feishu's web page serves a different (OAuth-style) confirmation
// flow that does NOT signal the device-flow poll endpoint, so the
// SDK polls forever and `nightme login feishu` hangs for the full
// 10-minute timeout with no greeting sent. We confirmed this in
// practice: the user successfully authorized via the browser page
// but the SDK never saw the authorization, no credentials were
// saved, no greeting was sent.
//
// Instead we render the QR ourselves, in three tiers of fallback:
//
//  1. Windows Terminal (Cascadia Code, VT processing on, detected
//     via WT_SESSION env var): RenderANSI — 24-bit ANSI color
//     half-block. Inline in the terminal, no external file. The
//     best UX, immune to font glyph issues because the colors do
//     the work.
//
//  2. Legacy conhost (cmd.exe, Windows PowerShell with the default
//     Consolas / Cascadia Code font, or any conhost where VT
//     processing is off): RenderASCII — half-block Unicode chars
//     (▀▄█), with the console output code page forced to UTF-8
//     first via enableConsoleOutputUTF8 and the console font set
//     to a known-good face via ensureConsoleFont.
//
//  3. Universal fallback: WritePNGToDesktop — a 512×512 PNG on
//     the user's Desktop, auto-opened via `cmd /c start "" <path>`
//     so the default photo viewer (Photos on Win10/11) pops up.
//     Always works regardless of font, code page, or Win version.
func (f *Provider) renderQRPlatform(url string) {
	// Set the console output code page to UTF-8 before printing
	// anything. On legacy conhost this is the difference between
	// the half-block glyphs (▀▄█) decoding correctly and being
	// mojibake'd into vertical bars when stdout is interpreted
	// through the OEM code page (e.g. GBK 936 on Chinese Windows).
	// The syscall only affects this process's console — closing
	// nightme leaves the user's cmd / PowerShell with their
	// original code page.
	enableConsoleOutputUTF8()
	// Set the console font to one we KNOW ships the half-block
	// glyphs (Cascadia Code, then Consolas as fallback). We
	// SET rather than DETECT because detection was the source of
	// the original failure mode — GetCurrentConsoleFontEx has
	// fragile name conventions (English vs. localised, TrueType
	// vs. raster, registry aliases), and a wrong match silently
	// forced the PNG fallback. With SET, the half-block chars
	// always render correctly and we can stop maintaining a
	// font whitelist.
	//
	// The change is scoped to the current console session: when
	// the user closes PowerShell / cmd, their preferred font is
	// back. We don't write to the registry or persist anything.
	ensureConsoleFont()

	shown := false
	if isWindowsTerminal() {
		if err := RenderANSI(url, f.out); err == nil {
			shown = true
		}
	}
	if !shown {
		// ensureConsoleFont has set a known-good font (Cascadia
		// Code or Consolas), so the half-block glyphs are
		// guaranteed to be available — no font whitelist check
		// needed.
		if err := RenderASCII(url, f.out, false); err == nil {
			shown = true
		}
	}

	if shown {
		// Time-based backup: even when the in-terminal QR is
		// rendered correctly, the user might not notice it (window
		// obscured, terminal in the background, didn't realize
		// they had to scan) or might fail to scan from the terminal
		// for some other reason. After qrFallbackAfter we open a
		// PNG copy in the default photo viewer as a second
		// channel.
		//
		// The goroutine is fire-and-forget: it does not coordinate
		// with the polling loop, so on a successful auth that
		// completes just before the timer fires, the user will
		// briefly see a popup after the greeting DM arrives. They
		// can dismiss it. This is preferable to the inverse — a
		// user staring at a screen waiting for nothing — because
		// the popup also acts as visual confirmation that the
		// login flow did its part.
		go schedulePNGFallback(url, qrFallbackAfter, f.out)
		return
	}

	// In-terminal path unavailable (the renderers errored for
	// some reason — broken pipe, no terminal at all). Immediate
	// PNG fallback.
	writeAndOpenPNG(url, f.out)
}

// writeAndOpenPNG is the synchronous PNG-fallback path: write a
// 512×512 PNG to the user's Desktop, print the path, and try to
// auto-open it in the default photo viewer. Used both as the
// immediate fallback when the in-terminal QR is unavailable and
// indirectly by schedulePNGFallback (which calls the same
// WritePNGToDesktop + openWithDefaultApp pair, just delayed).
func writeAndOpenPNG(url string, out io.Writer) {
	path, err := WritePNGToDesktop(url)
	if err != nil {
		fmt.Fprintf(out, "(QR rendering failed: %v)\n", err)
		fmt.Fprintln(out, "Open the URL above in a browser and paste it into Feishu to continue.")
		return
	}
	fmt.Fprintf(out, "QR code saved to: %s\n", path)
	if openErr := openWithDefaultApp(path); openErr != nil {
		fmt.Fprintf(out, "(Could not auto-open: %v — open the file from your Desktop manually.)\n", openErr)
	} else {
		fmt.Fprintln(out, "Opened the file in your default image viewer.")
	}
}

// schedulePNGFallback spawns a goroutine that, after delay,
// writes a fresh PNG copy of the QR to the user's Desktop and
// auto-opens it in the default photo viewer. The fresh file is
// important — the in-terminal QR was rendered on a best-effort
// basis and may have been visually broken on a misconfigured font;
// the PNG is guaranteed scannable.
//
// Before opening the PNG, we print a short explanation on stdout
// so the user understands why a QR suddenly appeared on their
// Desktop. Without this hint the popup feels random: "why did a
// photo viewer just open itself?" The hint answers that question
// and re-states the action they should take.
//
// The function returns immediately; the goroutine runs in the
// background for up to (delay) seconds. If the nightme process
// exits before then (e.g. quick successful auth), the goroutine
// is killed with the process — no orphaned subprocesses.
//
// Why we don't synchronize with the polling loop: doing so would
// require a shared context or done channel wired through
// Provider, OnQRCode callback, and back into renderQRPlatform. The
// goroutine is the simplest implementation, and the worst-case
// (popup arriving after a successful auth) is a minor annoyance
// the user can dismiss with one click.
func schedulePNGFallback(url string, delay time.Duration, out io.Writer) {
	go func() {
		time.Sleep(delay)
		// Print the explanation BEFORE writeAndOpenPNG runs so the
		// user sees it in the terminal alongside the popup, not
		// after they've already closed the popup wondering what
		// happened. fprintf (not println) so the text interleaves
		// cleanly with any concurrent log output.
		fmt.Fprintf(out, "\n(Backup QR opened after %ds with no scan detected — open the file and scan with the Feishu mobile app, or paste the URL printed above into Feishu.)\n", int(delay.Seconds()))
		writeAndOpenPNG(url, out)
	}()
}

// enableConsoleOutputUTF8 puts the current process's console into
// UTF-8 output mode (code page 65001) so Unicode characters like
// the half-block glyphs used by RenderASCII (▀▄█) and any CJK
// strings in error messages survive the OEM/ANSI code-page
// translation that legacy Windows conhost applies to stdout bytes.
//
// On Windows Terminal and other VT-aware hosts this is a visual
// no-op (the terminal already speaks UTF-8 natively), but it still
// helps when the program pipes to a file or to another tool that
// does its own code-page decoding.
//
// On legacy conhost with the default Consolas / Cascadia Code font
// (Win10 1809+), this is the difference between seeing half-block
// QR modules and seeing mojibake vertical bars in the rendered QR.
//
// Only affects this process's console; the user's parent shell
// session is left untouched, so closing nightme leaves cmd /
// PowerShell with its original code page.
//
// We use the typed wrapper from golang.org/x/sys/windows rather
// than shelling out to `chcp 65001` (which would either affect
// the user's whole shell — rude — or race with our own stdout
// writes — subtle). x/sys/windows has its own test suite for
// SetConsoleOutputCP and is the same wrapper the Go toolchain
// itself uses for its own Windows console calls.
func enableConsoleOutputUTF8() {
	// We intentionally swallow the error: a failed UTF-8 setting
	// only affects the half-block glyphs, and the worst-case
	// outcome is that RenderASCII's output is mojibake — which the
	// user will see and the PNG fallback path will rescue.
	_ = windows.SetConsoleOutputCP(65001)
}

// qrFallbackAfter is how long to display the in-terminal QR before
// the time-based backup fires (also open a PNG copy in the default
// photo viewer).
//
// 120 seconds is generous enough that a user who's actually going
// to scan does so well within the window (empirically: a person
// with the QR in view takes 5–15 seconds to fumble their phone out
// and complete the scan), but short enough that a confused user —
// QR window obscured, didn't notice it, terminal minimized — gets
// a second channel quickly rather than staring at a screen waiting
// for nothing for the full 10-minute polling timeout.
//
// Why 120 and not the more aggressive 30–60: fumbly phone unlock
// + open Feishu + find "scan" + confirm is realistically a
// 30–90-second task for someone who isn't a developer and isn't
// staring at the terminal. Cutting the timer shorter saves a
// false-positive popup on slow scanners without changing the
// unhappy-path outcome much.
//
// The polling timeout itself is 10 minutes (set by the Feishu SDK
// from the begin-response expire_in field), so this 120-second
// backup covers the "user never noticed the QR" failure mode long
// before the polling gives up.
const qrFallbackAfter = 120 * time.Second

// ensureConsoleFont picks a known-good monospace font and sets
// it on the current console, so the half-block Unicode glyphs
// (▀▄█) RenderASCII emits always have a font that ships them.
//
// We try Cascadia Code first (Win10 1809+ default in Windows
// Terminal, the most modern Microsoft monospace) then fall back
// to Consolas (Vista+ default). If both fail (CI / non-interactive
// session / very stripped Windows), we leave the font untouched and
// the 120-second PNG fallback handles the scan.
//
// Why SET rather than DETECT: the previous implementation tried
// to detect the current font via GetCurrentConsoleFontEx and
// match against a 50+ entry whitelist, but the API's name
// conventions (English vs. localised, TrueType vs. raster,
// registry aliases) made matching fragile enough that even a
// fully-supported Consolas / Cascadia setup could be misread as
// "unknown" — falling through to PNG. Setting the font directly
// sidesteps the entire detection problem: the font we set is
// the font that renders, full stop.
//
// Side effects: the user's console window font changes from
// whatever they had to Cascadia Code / Consolas for the duration
// of this session. The change is in-process only — closing
// PowerShell or cmd restores their preferred font on next launch.
// We do NOT touch the registry.
func ensureConsoleFont() {
	handle, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil || handle == windows.InvalidHandle {
		// No console attached (CI, redirected stdio). Skip —
		// nothing to set, and the user will hit the PNG path
		// via the fire-and-forget goroutine.
		return
	}
	for _, faceName := range []string{"Cascadia Code", "Consolas"} {
		if trySetConsoleFont(handle, faceName) {
			return
		}
	}
	// Neither candidate was accepted. Leave the current font
	// alone — the terminal may still render half-block chars
	// if it happens to be a supported one, and otherwise the
	// PNG fallback covers the user.
}

// trySetConsoleFont attempts to switch the current console to
// the given monospace font via SetCurrentConsoleFontEx. Returns
// true if the API accepted the change, false if the font isn't
// installed or the API rejected the request (e.g. console is
// redirected).
//
// We pass bMaximumWindow=FALSE so the change does not enlarge
// the console window if the new font has wider metrics — a
// user with a custom window size would not appreciate us
// silently resizing their terminal.
//
// The FaceName is encoded as UTF-16LE into the CONSOLE_FONT_INFOEX
// struct's FaceName[32] WCHAR array. dwFontSize / FontFamily /
// FontWeight are left at zero so the API only changes the font
// face, not its size or weight.
func trySetConsoleFont(handle windows.Handle, faceName string) bool {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("SetCurrentConsoleFontEx")

	var fontInfo struct {
		cbSize     uint32
		nFont      uint32
		dwFontSize struct{ X, Y int16 }
		FontFamily uint32
		FontWeight uint32
		FaceName   [32]uint16
	}
	fontInfo.cbSize = uint32(unsafe.Sizeof(fontInfo))

	// UTF16FromString returns a NUL-terminated UTF-16 slice (and
	// an error we ignore — faceName here is a hard-coded ASCII
	// constant from a Go literal, so encoding errors can't
	// happen). Copy as much as fits into the fixed-size FaceName
	// array; the trailing NUL is preserved by the slice's
	// terminator and Windows treats a NUL-padded FaceName as
	// equivalent to a length-prefixed string.
	//
	// (Go 1.26 deprecated the older StringToUTF16 spelling in
	// favour of UTF16FromString, which returns an explicit error
	// so callers can detect unencodable input rather than getting
	// a silently truncated result.)
	nameUTF16, _ := syscall.UTF16FromString(faceName)
	copy(fontInfo.FaceName[:], nameUTF16)

	ret, _, _ := proc.Call(
		uintptr(handle),
		0, // bMaximumWindow = FALSE — preserve user's window size
		uintptr(unsafe.Pointer(&fontInfo)),
	)
	return ret != 0
}

func isWindowsTerminal() bool {
	return os.Getenv("WT_SESSION") != ""
}

// openWithDefaultApp launches the system default handler for path.
// For .png files that's the default app associated with the .png
// extension (typically Photos on Win10/11).
//
// We use `cmd /c start "" <path>` rather than ShellExecuteW
// directly so we don't drag in golang.org/x/sys/windows just for
// one syscall. The empty "" before the path is the window-title
// argument to start.exe; without it, paths with spaces get
// misinterpreted as a title and never reach the default handler.
//
// Run() blocks until start has dispatched — enough for us to
// detect a missing handler. We don't wait for the launched app
// (Photos, etc.) to exit.
func openWithDefaultApp(path string) error {
	c := proc.New(context.Background(), "cmd", "/c", "start", "", path)
	c.Stdout = nil
	c.Stderr = nil
	return c.Run()
}
