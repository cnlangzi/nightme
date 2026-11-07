//go:build windows

package codepage

import "golang.org/x/sys/windows"

// LocalACP returns the ANSI code page of the running Windows
// install — 936 on a zh-CN host, 1252 on a Western one.
func LocalACP() uint32 {
	return windows.GetACP()
}
