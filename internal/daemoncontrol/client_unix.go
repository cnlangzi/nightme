//go:build !windows

package daemoncontrol

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func RequestSocket(path, command string, timeout time.Duration, result any) error {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) || isConnectionRefused(err) {
			return ErrNotRunning
		}
		return fmt.Errorf("connect to nightme daemon: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if err := WriteRequest(conn, command); err != nil {
		return fmt.Errorf("send daemon request: %w", err)
	}
	if err := ReadResponse(conn, result); err != nil {
		return fmt.Errorf("read daemon response: %w", err)
	}
	return nil
}

func Ping(path string, timeout time.Duration) (bool, error) {
	var ready Ready
	if err := RequestSocket(path, "ping", timeout, &ready); err != nil {
		return false, err
	}
	return ready.Ready, nil
}

func GetStatus(path string, timeout time.Duration) (Status, error) {
	var status Status
	if err := RequestSocket(path, "status", timeout, &status); err != nil {
		return Status{}, err
	}
	return status, nil
}

// GetHealth returns the live WS lifecycle snapshot from the running
// daemon. The returned HealthPayload.Health is the raw JSON-encoded
// WSHealthSnapshot (caller-side unmarshal). Used by `nightme health`.
func GetHealth(path string, timeout time.Duration) (HealthPayload, error) {
	var payload HealthPayload
	if err := RequestSocket(path, "health", timeout, &payload); err != nil {
		return HealthPayload{}, err
	}
	return payload, nil
}

func Stop(path string, timeout time.Duration) error {
	return RequestSocket(path, "stop", timeout, nil)
}

func isConnectionRefused(err error) bool {
	return errors.Is(err, unix.ECONNREFUSED)
}
