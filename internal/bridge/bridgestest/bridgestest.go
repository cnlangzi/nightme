// Package bridgestest is the shared test-helper surface for the
// bridge packages. It builds a fake agent CLI that a bridge test
// spawns in place of the real CLI, so the test drives the whole
// spawn path for real: pipes, stderr drain, handshake.
//
// The fake is a compiled Go program rather than a shell script so
// the same helper works on the Windows CI job.
//
// Production code MUST NOT import this package.
package bridgestest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// GoToolchainAvailable reports whether the go toolchain is on PATH.
// A test that compiles the fake CLI skips when it is missing — the
// toolchain is not part of every test environment.
func GoToolchainAvailable() bool {
	_, err := exec.LookPath("go")
	return err == nil
}

// BuildFakeCLI writes source to a .go file in dir, compiles it with
// `go build -o <dir>/<name> <file>` (run with the working directory
// set to dir) and returns the path of the produced binary.
//
// On Windows the binary carries the platform's .exe suffix —
// CreateProcess resolves an extensionless application name to
// "<name>.exe", so an extensionless output would not be runnable.
//
// The source is compiled as a file list, so it needs no go.mod.
func BuildFakeCLI(dir, name, source string) (string, error) {
	file := filepath.Join(dir, name+".go")
	if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
		return "", fmt.Errorf("bridgestest: write %s: %w", file, err)
	}
	bin := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, file)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("bridgestest: build %s: %w\n%s", name, err, out)
	}
	return bin, nil
}

// FakeCLISource is the source of the fake agent CLI. Its behaviour
// is selected by the FAKE_BRIDGE_MODE environment variable, which
// the child inherits from the bridge's spawn environment. The fake
// ignores its argv — the three bridges pass different flags.
//
// Modes:
//
//	crash      — write a fatal line to stderr, sleep, exit 3
//	silent     — exit 0 without writing anything
//	resume     — write the canonical omp resume rejection to stderr,
//	             sleep, exit 0
//	rpc        — serve the NDJSON envelope the omp / pi bridges
//	             speak: get_state succeeds, new_session is rejected
//	rpc_reject — like rpc, but get_state is rejected too, so the
//	             boot handshake fails
//
// The crash / resume modes sleep ~100 ms after writing to stderr so
// the bridge's stderr drain has certainly read the line before the
// child dies — the assertion is deterministic.
const FakeCLISource = `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

func main() {
	switch os.Getenv("FAKE_BRIDGE_MODE") {
	case "crash":
		fmt.Fprintln(os.Stderr, "fake bridge: fatal: cannot load provider config")
		time.Sleep(100 * time.Millisecond)
		os.Exit(3)
	case "silent":
		os.Exit(0)
	case "resume":
		fmt.Fprintln(os.Stderr, "Error: Session \"fake-session\" not found.")
		time.Sleep(100 * time.Millisecond)
		os.Exit(0)
	case "rpc":
		serve(false)
	case "rpc_reject":
		serve(true)
	default:
		os.Exit(0)
	}
}

// serve answers the NDJSON envelope the omp / pi bridges speak:
// one JSON object per line, requests carry an id, responses echo it
// back.
func serve(rejectGetState bool) {
	fmt.Fprintln(os.Stderr, "fake bridge: rpc mode ready")
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	out := bufio.NewWriter(os.Stdout)
	for in.Scan() {
		line := in.Bytes()
		if len(line) == 0 {
			continue
		}
		req := map[string]json.RawMessage{}
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		id := req["id"]
		command := ""
		if raw, ok := req["type"]; ok {
			_ = json.Unmarshal(raw, &command)
		}
		switch command {
		case "get_state":
			if rejectGetState {
				respond(out, id, "get_state", false, nil, "fake bridge: get_state refused")
				continue
			}
			respond(out, id, "get_state", true, map[string]any{"sessionId": "fake-1"}, "")
		case "new_session":
			respond(out, id, "new_session", false, nil, "fake bridge: new_session refused")
		case "abort":
			respond(out, id, "abort", true, nil, "")
		}
	}
}

// respond writes one response envelope and flushes it — the bridge
// waits for the line before it issues the next command.
func respond(out *bufio.Writer, id json.RawMessage, command string, ok bool, data any, message string) {
	env := map[string]any{
		"type":    "response",
		"command": command,
		"success": ok,
	}
	if len(id) > 0 {
		env["id"] = id
	}
	if data != nil {
		env["data"] = data
	}
	if message != "" {
		env["error"] = message
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return
	}
	out.Write(raw)
	out.WriteByte('\n')
	out.Flush()
}
`
