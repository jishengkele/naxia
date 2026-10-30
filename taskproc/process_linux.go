//go:build linux

package taskproc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func identity(pid int) (uint64, string, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, "", err
	}
	// comm may itself contain spaces and ')' characters.
	end := strings.LastIndex(string(raw), ") ")
	if end < 0 {
		return 0, "", fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(raw)[end+2:])
	if len(fields) < 20 {
		return 0, "", fmt.Errorf("short process stat")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	return start, fields[0], err
}

func marker(pid int) (string, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return "", err
	}
	value := ""
	for _, entry := range strings.Split(string(raw), "\x00") {
		if strings.HasPrefix(entry, EnvironmentKey+"=") {
			if value != "" {
				return "", fmt.Errorf("ambiguous process marker")
			}
			value = strings.TrimPrefix(entry, EnvironmentKey+"=")
		}
	}
	return value, nil
}

func (r *Registry) List(taskID int64) ([]Process, error) {
	scopes, err := r.scopes(taskID)
	if err != nil {
		return nil, err
	}
	if len(scopes) == 0 {
		return nil, nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []Process
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		start, state, err := identity(pid)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if state == "Z" || state == "X" {
			continue
		}
		token, err := marker(pid)
		// Inaccessible processes cannot be claimed. The independent writer scan
		// will still fail closed if they touch any archive source.
		if os.IsNotExist(err) || os.IsPermission(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		s, ok := scopes[token]
		if !ok {
			continue
		}
		again, _, err := identity(pid)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if again != start {
			continue
		}
		name, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "comm"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, Process{PID: pid, StartTicks: start, Name: strings.TrimSpace(string(name)), TaskID: taskID, SessionID: s.SessionID, IntentID: s.IntentID, token: token})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}

// A pidfd pins a particular process, eliminating the PID-reuse race between
// identity validation and signalling. Unsupported kernels fail safely.
func signalTerm(p Process) error {
	if p.PID <= 1 || p.PID == os.Getpid() {
		return fmt.Errorf("refusing application/init process")
	}
	fd, err := unix.PidfdOpen(p.PID, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	start, state, err := identity(p.PID)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if state == "Z" || state == "X" {
		return nil
	}
	token, err := marker(p.PID)
	if err != nil {
		return err
	}
	if start != p.StartTicks || token != p.token {
		return fmt.Errorf("process %d identity changed; not stopped", p.PID)
	}
	err = unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

// Stop waits for normal termination, discovering late descendants on every
// pass. It never escalates to SIGKILL or signals a group/unknown process.
func (r *Registry) Stop(ctx context.Context, taskID int64) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	sent := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("后台服务未正常退出，原数据保留（已停止的服务不会自动重启）: %w", err)
		}
		processes, err := r.List(taskID)
		if err != nil {
			return err
		}
		if len(processes) == 0 {
			return nil
		}
		for _, p := range processes {
			key := fmt.Sprintf("%d:%d", p.PID, p.StartTicks)
			if sent[key] {
				continue
			}
			if err := signalTerm(p); err != nil {
				return fmt.Errorf("停止任务 %d 后台进程 %d: %w", taskID, p.PID, err)
			}
			sent[key] = true
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}
