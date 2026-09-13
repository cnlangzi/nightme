// install_hook.go — bridge between host/ and parent dsh/ package
// to install the host waterfall handler at host Client construction
// time, before the WS pump opens the connection. dsh sends the
// `ready` frame immediately after WS upgrade, so the handler must
// be wired before cli.Start(ctx).
//
// The hook is a process-once function variable — the parent dsh
// package registers it during init (importing the host package
// triggers the side effect via the `import _` in dsh/doc.go or
// dsh/host_waterfall.go).
package host

// OnLifecycleInstall runs the package-level host handler install
// hook (registered by the parent dsh package via SetLifecycleInstall)
// against the freshly-constructed Client. It is safe to call before
// or after cli.Start; the parent dsh package makes the hook
// idempotent and process-once.
//
// Lifecycle ordering matters: spawnAndWire calls this right after
// constructing the Client and BEFORE cli.Start, so the `ready`
// frame dsh sends on the new WS arrives at a host handler that is
// already installed.
func OnLifecycleInstall(c *Client) {
	if c == nil {
		return
	}
	if lifecycleInstallHook != nil {
		lifecycleInstallHook(c)
	}
}

// SetLifecycleInstall registers the hook. The parent dsh package
// calls this exactly once during package init. Subsequent calls
// panic — the hook is meant to be a single, package-bound function.
func SetLifecycleInstall(fn func(*Client)) {
	if lifecycleInstallHook != nil {
		panic("host: SetLifecycleInstall called twice")
	}
	if fn == nil {
		panic("host: SetLifecycleInstall called with nil fn")
	}
	lifecycleInstallHook = fn
}

// UnsetLifecycleInstall removes the hook. Tests only.
func UnsetLifecycleInstall() {
	lifecycleInstallHook = nil
}

var lifecycleInstallHook func(*Client)
