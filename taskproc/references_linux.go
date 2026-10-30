//go:build linux

package taskproc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func vanished(err error) bool { return os.IsNotExist(err) || errors.Is(err, unix.ESRCH) }

// References checks working directories, executables, arguments and all open
// files, including read-only/deleted descriptors. A browser can keep rendering
// without an open writable file. Permission errors are not proof of quiescence.
func References(roots []string) ([]Reference, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	for _, root := range roots {
		if !filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator) {
			return nil, fmt.Errorf("invalid task process reference root %q", root)
		}
	}
	processes, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var result []Reference
	for _, entry := range processes {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		start, state, err := identity(pid)
		if vanished(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if state == "Z" || state == "X" {
			continue
		}
		base := filepath.Join("/proc", entry.Name())
		paths := map[string]bool{}
		add := func(kind, target string) {
			target = strings.TrimSuffix(target, " (deleted)")
			for _, root := range roots {
				root = filepath.Clean(root)
				if target == root || strings.HasPrefix(target, root+string(filepath.Separator)) {
					paths[kind+target] = true
					break
				}
			}
		}
		for _, kind := range []string{"cwd", "exe"} {
			target, err := os.Readlink(filepath.Join(base, kind))
			if vanished(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("检查进程 %d %s: %w", pid, kind, err)
			}
			add(kind+": ", target)
		}
		args, err := os.ReadFile(filepath.Join(base, "cmdline"))
		if vanished(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, arg := range strings.Split(string(args), "\x00") {
			// Report only the referenced task root, never the entire command,
			// which may contain credentials or other unrelated arguments.
			for _, root := range roots {
				for offset := 0; offset < len(arg); {
					i := strings.Index(arg[offset:], root)
					if i < 0 {
						break
					}
					i += offset
					end := i + len(root)
					if end == len(arg) || strings.ContainsRune("/\\ \t\r\n\"'", rune(arg[end])) {
						paths["参数引用: "+root] = true
						break
					}
					offset = end
				}
			}
		}
		fds, err := os.ReadDir(filepath.Join(base, "fd"))
		if vanished(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("检查进程 %d 文件引用: %w", pid, err)
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(base, "fd", fd.Name()))
			if vanished(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			add("fd: ", target)
		}
		if len(paths) == 0 {
			continue
		}
		again, _, err := identity(pid)
		if vanished(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if start != again {
			return nil, fmt.Errorf("进程 %d 在检查期间发生变化，请重试删除", pid)
		}
		name, err := os.ReadFile(filepath.Join(base, "comm"))
		if vanished(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		r := Reference{PID: pid, StartTicks: start, Name: strings.TrimSpace(string(name))}
		for path := range paths {
			r.Paths = append(r.Paths, path)
		}
		sort.Strings(r.Paths)
		result = append(result, r)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].PID < result[j].PID })
	return result, nil
}
