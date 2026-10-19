//go:build !windows

package pty

import (
	"fmt"

	gopty "github.com/aymanbagabas/go-pty"
	"golang.org/x/sys/unix"
)

// setRaw puts the slave into raw mode. A new PTY is in canonical
// mode, and Linux delivers at most 4096 bytes of one line, dropping
// the rest up to the newline. ACP writes each JSON-RPC message as a
// single line; a session/prompt of a few tens of kilobytes is then
// truncated, the agent never sees a valid request, and the client
// waits forever for the response.
func setRaw(p gopty.Pty) error {
	u, ok := p.(gopty.UnixPty)
	if !ok {
		return fmt.Errorf("pty: unix pty required")
	}
	fd := int(u.Slave().Fd())
	termios, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return fmt.Errorf("pty: get termios: %w", err)
	}
	termios.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	termios.Oflag &^= unix.OPOST
	termios.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	termios.Cflag &^= unix.CSIZE | unix.PARENB
	termios.Cflag |= unix.CS8
	termios.Cc[unix.VMIN] = 1
	termios.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, termios); err != nil {
		return fmt.Errorf("pty: set raw: %w", err)
	}
	return nil
}
