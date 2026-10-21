//go:build !windows

package omp

import "os"

func writeFileOS(path, content string, mode os.FileMode) error {
	return os.WriteFile(path, []byte(content), mode)
}
