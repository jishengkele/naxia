package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/taskproc"
)

type taskDeleteProcessConflict struct {
	References []taskproc.Reference `json:"processes,omitempty"`
	Err        error                `json:"-"`
}

func (e *taskDeleteProcessConflict) Error() string {
	var details []string
	for _, r := range e.References {
		details = append(details, fmt.Sprintf("PID %d (%s): %s", r.PID, r.Name, strings.Join(r.Paths, ", ")))
	}
	if e.Err != nil {
		details = append(details, e.Err.Error())
	}
	return "任务后台进程尚未安全收尾，删除已取消；任务文件和数据库未提交删除。请先核实未知或共享进程；已停止的服务不会自动重启：" + strings.Join(details, "；")
}
func (e *taskDeleteProcessConflict) Unwrap() error { return e.Err }

func taskDeleteProcessRoots(dataDir, id string, expID int64) ([]string, error) {
	base, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	roots := []string{filepath.Join(base, "tasks", id)}
	if expID > 0 {
		prefix := fmt.Sprintf("exp%d-", expID)
		for _, kind := range []string{"transcripts", "sessions", "noa"} {
			entries, err := os.ReadDir(filepath.Join(base, kind))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), prefix) {
					roots = append(roots, filepath.Join(base, kind, entry.Name()))
				}
			}
		}
	}
	// Include recoverable remnants from previous deletion attempts without
	// interpreting a pathname as authority to terminate any process.
	entries, err := os.ReadDir(filepath.Join(base, ".delete-staging"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "task-"+id+"-") {
			roots = append(roots, filepath.Join(base, ".delete-staging", entry.Name()))
		}
	}
	return roots, nil
}

func prepareTaskDeleteProcesses(ctx context.Context, registry *taskproc.Registry, id int64, roots []string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, &taskDeleteProcessConflict{Err: err}
	}
	owned, err := registry.List(id)
	if err != nil {
		return false, &taskDeleteProcessConflict{Err: err}
	}
	refs, err := taskproc.References(roots)
	if err != nil {
		return false, &taskDeleteProcessConflict{Err: err}
	}
	var unknown []taskproc.Reference
	for _, r := range refs {
		known := false
		for _, p := range owned {
			if p.PID == r.PID && p.StartTicks == r.StartTicks {
				known = true
				break
			}
		}
		if !known {
			unknown = append(unknown, r)
		}
	}
	if len(unknown) > 0 {
		return false, &taskDeleteProcessConflict{References: unknown}
	}
	stopped := len(owned) > 0
	if err := registry.Stop(ctx, id); err != nil {
		return stopped, &taskDeleteProcessConflict{Err: err}
	}
	return stopped, ensureTaskDeleteProcessesQuiet(ctx, registry, id, roots)
}

func ensureTaskDeleteProcessesQuiet(ctx context.Context, registry *taskproc.Registry, id int64, roots []string) error {
	if err := ctx.Err(); err != nil {
		return &taskDeleteProcessConflict{Err: err}
	}
	owned, err := registry.List(id)
	if err != nil {
		return &taskDeleteProcessConflict{Err: err}
	}
	if len(owned) > 0 {
		var names []string
		for _, p := range owned {
			names = append(names, "PID "+strconv.Itoa(p.PID)+" ("+p.Name+")")
		}
		return &taskDeleteProcessConflict{Err: fmt.Errorf("已登记后台进程仍存活: %s", strings.Join(names, ", "))}
	}
	refs, err := taskproc.References(roots)
	if err != nil {
		return &taskDeleteProcessConflict{Err: err}
	}
	if len(refs) > 0 {
		return &taskDeleteProcessConflict{References: refs}
	}
	return nil
}
