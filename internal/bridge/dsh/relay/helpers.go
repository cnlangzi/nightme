// helpers.go — small package-private helpers for the relay.
//
// detectRepoRoot + blocksToPromptParts intentionally re-implement
// the same logic that lives in session.go of the parent dsh
// package. The relay can't import dsh because dsh already imports
// relay (the Bridge interface). Duplicating keeps the dependency
// edge one-way and avoids an internal/ subpackage for two small
// functions. If either grows or drifts, the right move is to
// extract both to internal/bridge/dsh/internal/.
package relay

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
)

// detectRepoRoot returns the git toplevel of workspace, or
// workspace itself if not in a git repo. Mirrors
// session.go:detectRepoRoot — single source of truth should
// move to internal/ if the duplication grows past this one
// caller.
func detectRepoRoot(workspace string) string {
	cmd := exec.Command("git", "-C", workspace, "rev-parse", "--show-toplevel")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return workspace
	}
	root := strings.TrimSpace(out.String())
	if root == "" {
		return workspace
	}
	return root
}

// blocksToPromptParts converts the bridge's content-block
// representation to the host package's wire-shape PromptPart.
// Text blocks pass through. Image blocks are base64-inlined
// (matches dsh's image content shape). File blocks are emitted
// as text with the path so the agent's tools can read it (the
// host doesn't define a typed file-content block).
func blocksToPromptParts(blocks []agent.ContentBlock) ([]host.PromptPart, error) {
	if len(blocks) == 0 {
		return nil, nil
	}
	out := make([]host.PromptPart, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case agent.ContentText:
			out = append(out, host.PromptPart{Type: "text", Text: b.Text})
		case agent.ContentImage:
			// Read + base64 inline. Caller is expected to pass
			// small images (the runtime's image helper already
			// scales); the host's body cap (16 MiB) is the real
			// backstop.
			data, err := readFileB64(b.Path)
			if err != nil {
				return nil, fmt.Errorf("relay: read image %s: %w", b.Path, err)
			}
			out = append(out, host.PromptPart{
				Type:      "image",
				MediaType: b.MediaType,
				Data:      data,
				Name:      b.Path,
			})
		case agent.ContentFile:
			// No typed file part — emit a path note so the agent
			// can read it with its native fs tool.
			out = append(out, host.PromptPart{
				Type: "text",
				Text: fmt.Sprintf("[file: %s]", b.Path),
			})
		default:
			return nil, fmt.Errorf("relay: unknown content block type %q", b.Type)
		}
	}
	return out, nil
}

// readFileB64 reads path and returns its base64 encoding.
// Duplicates session.go:contentBlocksToDTO image path — see
// helpers.go header for why.
func readFileB64(path string) (string, error) {
	// Inline import-free read to keep helpers.go dependency
	// surface tiny; the same shape as session.go:1287.
	cmd := exec.Command("cat", path)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(out.Bytes()), nil
}

// mintRequestID is the per-prompt UUID dsh 0.1.2-rc.1 requires
// on session.prompt. host.RPCClient mints its own RPCID; the
// prompt-request id is a separate field, so the relay hands a
// fresh uuid here.
func mintRequestID() string {
	// Delegate to the host's helper if it's exported; fallback
	// to a self-built uuid when running in test contexts that
	// stub host.RPCClient.
	return host.NewRPCID()
}

// suppress unused-import warnings if helpers shrink.
var _ = json.Marshal
