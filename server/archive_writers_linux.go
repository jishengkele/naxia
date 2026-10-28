//go:build linux

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Never kill an unregistered process based on a name or a path. A legacy
// detached writer is an explicit blocker, including descriptors whose path was
// renamed/deleted. Scanning failures are not evidence that a file is quiescent.
func archiveCheckWriters(roots []string) error {
	if len(roots) == 0 {
		return nil
	}
	processes, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	for _, process := range processes {
		pid, err := strconv.Atoi(process.Name())
		if err != nil {
			continue
		}
		base := filepath.Join("/proc", process.Name())
		fds, err := os.ReadDir(filepath.Join(base, "fd"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("无法检查进程 %d 的归档写入: %w", pid, err)
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(base, "fd", fd.Name()))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			target = strings.TrimSuffix(target, " (deleted)")
			matched := false
			for _, root := range roots {
				if target == root || strings.HasPrefix(target, root+string(os.PathSeparator)) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(base, "fdinfo", fd.Name()))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			for _, line := range strings.Split(string(raw), "\n") {
				if !strings.HasPrefix(line, "flags:") {
					continue
				}
				flags, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "flags:")), 8, 64)
				if err != nil {
					return err
				}
				if flags&3 != 0 {
					return fmt.Errorf("任务文件仍被后台进程写入：PID %d，%s；请先停止该任务的后台服务，再重试归档", pid, target)
				}
			}
		}
	}
	return nil
}
