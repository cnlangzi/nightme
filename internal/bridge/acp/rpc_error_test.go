package acp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRPCError_IncludesData(t *testing.T) {
	e := &rpcError{
		Code:    -32602,
		Message: "Invalid params",
		Data:    json.RawMessage(`{"message":"Session \"abc\" not found"}`),
	}
	got := e.Error()
	if !strings.Contains(got, "Invalid params") || !strings.Contains(got, "not found") {
		t.Fatalf("Error() = %q, want message and data", got)
	}
}

func TestRPCError_OmitsEmptyData(t *testing.T) {
	e := &rpcError{Code: -32602, Message: "Invalid params"}
	got := e.Error()
	if strings.Contains(got, ": {") {
		t.Fatalf("Error() = %q, want message only", got)
	}
}
