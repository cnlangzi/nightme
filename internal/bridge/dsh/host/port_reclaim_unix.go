//go:build !windows

package host

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func listenerPIDs(port int) ([]int, error) {
	if runtime.GOOS == "linux" {
		return listenerPIDsProc(port)
	}
	return listenerPIDsLsof(port)
}

func killPID(pid int) error {
	err := syscall.Kill(pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func listenerPIDsLsof(port int) ([]int, error) {
	cmd := exec.Command("lsof", "-nP",
		"-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-t")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && strings.TrimSpace(string(out)) == "" {
			return nil, nil
		}
		return nil, fmt.Errorf("lsof: %w", err)
	}
	return parsePIDs(string(out))
}

func listenerPIDsProc(port int) ([]int, error) {
	inodes := map[string]struct{}{}
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		body, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		lines := strings.Split(string(body), "\n")
		for i, line := range lines {
			if i == 0 || line == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			local := fields[1]
			colon := strings.LastIndex(local, ":")
			if colon < 0 {
				continue
			}
			p, err := strconv.ParseUint(local[colon+1:], 16, 16)
			if err != nil || int(p) != port {
				continue
			}
			inodes[fields[9]] = struct{}{}
		}
	}
	if len(inodes) == 0 {
		return nil, nil
	}
	return pidsForSocketInodes(inodes)
}

func pidsForSocketInodes(inodes map[string]struct{}) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	seen := map[int]struct{}{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", entry.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			if !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
				continue
			}
			ino := target[len("socket:[") : len(target)-1]
			if _, ok := inodes[ino]; !ok {
				continue
			}
			if _, ok := seen[pid]; ok {
				break
			}
			seen[pid] = struct{}{}
			pids = append(pids, pid)
			break
		}
	}
	return pids, nil
}

func parsePIDs(out string) ([]int, error) {
	var pids []int
	seen := map[int]struct{}{}
	for _, line := range strings.Fields(out) {
		pid, err := strconv.Atoi(line)
		if err != nil {
			return nil, fmt.Errorf("parse pid %q: %w", line, err)
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		pids = append(pids, pid)
	}
	return pids, nil
}
