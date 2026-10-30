//go:build linux

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Never kill an unregistered process based on a name or a path. A legacy
// detached writer is an explicit blocker, including descriptors whose path was
// renamed/deleted. Scanning failures are not evidence that a file is quiescent.
func archiveListWriters(roots []string) ([]archiveWriter, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	processes, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var writers []archiveWriter
	for _, process := range processes {
		pid, err := strconv.Atoi(process.Name())
		if err != nil {
			continue
		}
		base := filepath.Join("/proc", process.Name())
		before, err := archiveProcessStart(base)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		fds, err := os.ReadDir(filepath.Join(base, "fd"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("无法检查进程 %d 的归档写入: %w", pid, err)
		}
		files := map[string]bool{}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(base, "fd", fd.Name()))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
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
				return nil, err
			}
			foundFlags := false
			for _, line := range strings.Split(string(raw), "\n") {
				if !strings.HasPrefix(line, "flags:") {
					continue
				}
				flags, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "flags:")), 8, 64)
				if err != nil {
					return nil, err
				}
				foundFlags = true
				if flags&3 != 0 {
					files[target] = true
				}
			}
			if !foundFlags {
				return nil, fmt.Errorf("无法读取进程 %d 文件 %s 的访问模式", pid, target)
			}
		}
		if len(files) > 0 {
			after, err := archiveProcessStart(base)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if before != after {
				return nil, fmt.Errorf("进程 %d 在写入检查期间发生变化，请重试归档", pid)
			}
			name, err := os.ReadFile(filepath.Join(base, "comm"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			w := archiveWriter{PID: pid, StartTicks: before, Name: strings.TrimSpace(string(name))}
			for path := range files {
				w.Files = append(w.Files, path)
			}
			sort.Strings(w.Files)
			writers = append(writers, w)
		}
	}
	sort.Slice(writers, func(i, j int) bool { return writers[i].PID < writers[j].PID })
	return writers, nil
}

func archiveProcessStart(base string) (uint64, error) {
	raw, err := os.ReadFile(filepath.Join(base, "stat"))
	if err != nil {
		return 0, err
	}
	end := strings.LastIndex(string(raw), ") ")
	if end < 0 {
		return 0, fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(raw)[end+2:])
	if len(fields) < 20 {
		return 0, fmt.Errorf("short process stat")
	}
	return strconv.ParseUint(fields[19], 10, 64)
}
