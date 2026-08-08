package daemoncontrol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	ProtocolVersion = 1
	maxFrameSize    = 64 << 10
)

type Request struct {
	Version int    `json:"version"`
	Command string `json:"command"`
}

type Response struct {
	Version int             `json:"version"`
	OK      bool            `json:"ok"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   string          `json:"error,omitempty"`
}

type Status struct {
	ProtocolVersion int       `json:"protocol_version"`
	PID             int       `json:"pid"`
	State           string    `json:"state"`
	StartedAt       time.Time `json:"started_at"`
	UptimeSeconds   int64     `json:"uptime_seconds"`
	Channel         string    `json:"channel"`
	Version         string    `json:"version"`
	LogPath         string    `json:"log_path,omitempty"`
}

// HealthPayload is the response body for the "health" RPC command.
// Wraps a JSON-encoded WSHealthSnapshot as RawMessage so the wire
// format stays loose-coupled: the daemoncontrol package doesn't
// import channel/feishu (which would create an import cycle).
type HealthPayload struct {
	Channel string          `json:"channel"`
	Health  json.RawMessage `json:"health"`
}

type Ready struct {
	Ready bool `json:"ready"`
}

func WriteRequest(w io.Writer, command string) error {
	return writeJSON(w, Request{Version: ProtocolVersion, Command: command})
}

func ReadRequest(r io.Reader) (Request, error) {
	var req Request
	if err := readJSON(r, &req); err != nil {
		return Request{}, err
	}
	if req.Version != ProtocolVersion {
		return Request{}, fmt.Errorf("unsupported protocol version %d", req.Version)
	}
	if req.Command == "" {
		return Request{}, errors.New("missing command")
	}
	return req, nil
}

func WriteResult(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeJSON(w, Response{Version: ProtocolVersion, OK: true, Result: data})
}

func WriteError(w io.Writer, err error) error {
	message := "unknown error"
	if err != nil {
		message = err.Error()
	}
	return writeJSON(w, Response{Version: ProtocolVersion, OK: false, Error: message})
}

func ReadResponse(r io.Reader, value any) error {
	var resp Response
	if err := readJSON(r, &resp); err != nil {
		return err
	}
	if resp.Version != ProtocolVersion {
		return fmt.Errorf("unsupported protocol version %d", resp.Version)
	}
	if !resp.OK {
		if resp.Error == "" {
			resp.Error = "daemon request failed"
		}
		return errors.New(resp.Error)
	}
	if value == nil || len(resp.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Result, value); err != nil {
		return fmt.Errorf("decode daemon response: %w", err)
	}
	return nil
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(value)
}

func readJSON(r io.Reader, value any) error {
	line, err := bufio.NewReader(io.LimitReader(r, maxFrameSize+1)).ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read daemon response: %w", err)
	}
	if len(line) > maxFrameSize {
		return errors.New("daemon control frame is too large")
	}
	if err := json.Unmarshal(line, value); err != nil {
		return fmt.Errorf("decode daemon control frame: %w", err)
	}
	return nil
}
