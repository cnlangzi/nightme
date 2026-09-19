//go:build windows

package host

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

func listenerPIDs(port int) ([]int, error) {
	out, err := exec.Command("netstat", "-ano", "-p", "TCP").Output()
	if err != nil {
		return nil, fmt.Errorf("netstat: %w", err)
	}
	suffix := ":" + strconv.Itoa(port)
	var pids []int
	seen := map[int]struct{}{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if !strings.EqualFold(fields[3], "LISTENING") {
			continue
		}
		if !strings.HasSuffix(fields[1], suffix) {
			continue
		}
		pid, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		pids = append(pids, pid)
	}
	return pids, nil
}

func killPID(pid int) error {
	err := exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
	if err != nil {
		return fmt.Errorf("taskkill %d: %w", pid, err)
	}
	return nil
}
