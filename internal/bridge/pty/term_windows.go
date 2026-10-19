//go:build windows

package pty

import gopty "github.com/aymanbagabas/go-pty"

func setRaw(p gopty.Pty) error {
	return nil
}
