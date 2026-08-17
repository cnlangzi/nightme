// testdata/cursor-stub — a minimal stub binary that simulates
// cursor's `agent -p` print-mode behavior for testing.
//
// Behavior is controlled by the CURSOR_STUB_BEHAVIOR env var:
//
//   - "happy"     → prints CURSOR_STUB_TEXT (default "READY"), exit 0
//   - "exit_1"    → prints CURSOR_STUB_TEXT to stderr, exit 1
//   - "empty"     → prints nothing, exit 0
//   - "echo"      → prints the last positional arg (the prompt), exit 0
//
// The stub reads CURSOR_STUB_TEXT from env for the output content.
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	behavior := os.Getenv("CURSOR_STUB_BEHAVIOR")
	text := os.Getenv("CURSOR_STUB_TEXT")
	if text == "" {
		text = "READY"
	}

	switch behavior {
	case "happy":
		fmt.Print(text)
	case "exit_1":
		fmt.Fprintln(os.Stderr, text)
		os.Exit(1)
	case "empty":
		// print nothing
	case "echo":
		// print the last positional arg (the prompt)
		args := os.Args[1:]
		// find "-p" and take the next arg
		for i, a := range args {
			if a == "-p" && i+1 < len(args) {
				fmt.Print(args[i+1])
				return
			}
		}
		// fallback: print all args
		fmt.Print(strings.Join(args, " "))
	default:
		fmt.Print(text)
	}
}
