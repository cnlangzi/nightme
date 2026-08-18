package tray

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/spf13/cobra"
)

func TestInvoke_NilCmd(t *testing.T) {
	if err := Invoke(nil); err != nil {
		t.Errorf("Invoke(nil) = %v, want nil", err)
	}
}

func TestInvoke_NoRunE(t *testing.T) {
	// Leaf commands with no RunE (defensive only — the registry
	// already filters Hidden commands out of TrayItems()).
	cmd := &cobra.Command{Use: "noop"}
	if err := Invoke(cmd); err != nil {
		t.Errorf("Invoke(cmd without RunE) = %v, want nil", err)
	}
}

func TestInvoke_RunsRunE(t *testing.T) {
	calls := 0
	cmd := &cobra.Command{
		Use: "ping",
		RunE: func(_ *cobra.Command, _ []string) error {
			calls++
			return nil
		},
	}
	if err := Invoke(cmd); err != nil {
		t.Fatalf("Invoke = %v, want nil", err)
	}
	if calls != 1 {
		t.Errorf("RunE called %d times, want 1", calls)
	}
}

func TestInvoke_PropagatesError(t *testing.T) {
	sentinel := errors.New("boom")
	cmd := &cobra.Command{
		Use: "fail",
		RunE: func(_ *cobra.Command, _ []string) error {
			return sentinel
		},
	}
	err := Invoke(cmd)
	if !errors.Is(err, sentinel) {
		t.Errorf("Invoke = %v, want %v", err, sentinel)
	}
}

func TestInvoke_DiscardsOutputAndRestoresWriters(t *testing.T) {
	customOut := &bytes.Buffer{}
	customErr := &bytes.Buffer{}
	cmd := &cobra.Command{
		Use: "loud",
		RunE: func(c *cobra.Command, _ []string) error {
			// RunE writes to its own OutOrStderr. If Invoke
			// didn't redirect, this would land in customOut
			// and trip the assertion below.
			c.OutOrStderr().Write([]byte("should be discarded\n"))
			return nil
		},
	}
	cmd.SetOut(customOut)
	cmd.SetErr(customErr)

	if err := Invoke(cmd); err != nil {
		t.Fatalf("Invoke = %v", err)
	}
	if customOut.Len() != 0 {
		t.Errorf("customOut got %q, want empty (Invoke should discard)", customOut.String())
	}
	if customErr.Len() != 0 {
		t.Errorf("customErr got %q, want empty", customErr.String())
	}
	// Writers must be restored so the next caller (or the REPL)
	// sees the original output destination, not a discarded
	// io.Discard that swallows their output.
	if cmd.OutOrStdout() != customOut {
		t.Error("OutOrStdout() not restored to caller writer")
	}
	if cmd.ErrOrStderr() != customErr {
		t.Error("ErrOrStderr() not restored to caller writer")
	}
	// Sanity: restored writer is functional.
	if _, err := io.WriteString(customOut, "after\n"); err != nil {
		t.Fatal(err)
	}
	if got := customOut.String(); got != "after\n" {
		t.Errorf("customOut after Invoke = %q, want %q", got, "after\n")
	}
}
