package server

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Autumn-27/artex/taskproc"
)

type archiveWriter struct {
	PID        int      `json:"pid"`
	StartTicks uint64   `json:"start_ticks"`
	Name       string   `json:"name"`
	Files      []string `json:"files"`
	Managed    bool     `json:"managed"`
	TaskID     int64    `json:"task_id,omitempty"`
}

type archiveWriterConflict struct{ Writers []archiveWriter }

func (e *archiveWriterConflict) Error() string {
	var details []string
	for _, w := range e.Writers {
		details = append(details, fmt.Sprintf("PID %d (%s): %s", w.PID, w.Name, strings.Join(w.Files, ", ")))
	}
	return "任务文件仍被后台进程写入：" + strings.Join(details, "；") + "；请先停止列出的后台服务，再重试归档；原数据保留"
}

type archiveServiceConflict struct{ Err error }

func (e *archiveServiceConflict) Error() string {
	return "归档后台服务收尾失败；原数据保留，已停止的服务不会自动重启：" + e.Err.Error()
}
func (e *archiveServiceConflict) Unwrap() error { return e.Err }

func archiveCheckWriters(roots []string) error {
	writers, err := archiveListWriters(roots)
	if err != nil {
		return err
	}
	if len(writers) > 0 {
		return &archiveWriterConflict{Writers: writers}
	}
	return nil
}

// Recheck ownership as well as open files: a late child may not have opened its
// next output file yet. Never commit while a registered task service is alive.
func (s *Server) checkTaskArchiveQuiescent(taskID int64, roots []string) error {
	if s.processes != nil {
		processes, err := s.processes.List(taskID)
		if err != nil {
			return err
		}
		if len(processes) > 0 {
			var names []string
			for _, p := range processes {
				names = append(names, fmt.Sprintf("PID %d (%s)", p.PID, p.Name))
			}
			return &archiveServiceConflict{Err: fmt.Errorf("任务后台进程仍存活：%s", strings.Join(names, "；"))}
		}
	}
	return archiveCheckWriters(roots)
}

func (s *Server) archiveProcessState(taskID int64, sources []archiveSource) ([]taskproc.Process, []archiveWriter, error) {
	var processes []taskproc.Process
	if s.processes != nil {
		var err error
		processes, err = s.processes.List(taskID)
		if err != nil {
			return nil, nil, err
		}
	}
	writers, err := archiveListWriters(archiveSourceRoots(sources))
	if err != nil {
		return nil, nil, err
	}
	for i := range writers {
		writers[i].TaskID = taskID
		for _, p := range processes {
			if p.PID == writers[i].PID && p.StartTicks == writers[i].StartTicks {
				writers[i].Managed = true
				break
			}
		}
	}
	sort.Slice(writers, func(i, j int) bool { return writers[i].PID < writers[j].PID })
	return processes, writers, nil
}

func unknownArchiveWriters(writers []archiveWriter) []archiveWriter {
	var unknown []archiveWriter
	for _, w := range writers {
		if !w.Managed {
			unknown = append(unknown, w)
		}
	}
	return unknown
}
