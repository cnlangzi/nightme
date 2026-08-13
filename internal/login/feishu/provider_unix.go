//go:build !windows

package feishu

// renderQRPlatform is the Unix/macOS/BSD rendering entry point.
// Windows Terminal with Cascadia Code handles the half-block
// glyphs natively, so this path also covers Windows users running
// inside a sane terminal — except the build tag excludes Windows
// outright, so Windows always goes through provider_windows.go's
// version (which dispatches ANSI vs PNG based on terminal).
//
// Errors here mean stdout is broken (closed pipe); nothing for the
// user to do, and registration.RegisterApp will still block on the
// polling loop.
func (f *Provider) renderQRPlatform(url string) {
	_ = RenderASCII(url, f.out, false)
}