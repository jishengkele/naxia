//go:build linux

package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/taskproc"
)

func archiveServiceFixture(t *testing.T, root string, env []string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "background.log")
	cmd := exec.Command("python3", "-c", `import os,sys,signal,time
f=open(sys.argv[1],'a')
def finish(*args):
 f.write('shutdown-final\n');f.flush();os.fsync(f.fileno());f.close();sys.exit(0)
signal.signal(signal.SIGTERM,finish)
f.write('ready\n');f.flush()
while True:time.sleep(.05)
`, path)
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if raw, _ := os.ReadFile(path); string(raw) == "ready\n" {
			return path
		}
		if time.Now().After(deadline) {
			t.Fatal("service did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestArchiveListsEveryUnknownWriterAndReturnsConflictWithoutMutation(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	root := filepath.Join(s.m.dir, "tasks", task.ID)
	a := archiveServiceFixture(t, filepath.Join(root, "service-a"), nil)
	b := archiveServiceFixture(t, filepath.Join(root, "service-b"), nil)
	w := httptest.NewRecorder()
	s.estimateTaskArchives(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"task_ids":["`+task.ID+`"]}`)))
	var preview archiveSpaceEstimate
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil || w.Code != 200 || len(preview.Writers) != 2 {
		t.Fatalf("preview=%s err=%v", w.Body.String(), err)
	}
	for _, writer := range preview.Writers {
		if writer.Managed {
			t.Fatal("legacy writer claimed as owned")
		}
	}
	w = httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("id", task.ID)
	s.queueTaskArchive(w, r)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "archive_writers") || !strings.Contains(w.Body.String(), "service-a") || !strings.Contains(w.Body.String(), "service-b") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	for _, path := range []string{a, b} {
		if raw, _ := os.ReadFile(path); string(raw) != "ready\n" {
			t.Fatalf("unknown service changed: %q", raw)
		}
	}
	if job, err := s.m.pg.GetTaskArchiveByTask(id); err != nil || job != nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if s.engine.IsDeleting(task.ID) {
		t.Fatal("barrier retained on writer conflict")
	}
}

func TestArchiveStopsOwnedServicesAndIncludesShutdownWrites(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	s.processes = &taskproc.Registry{Root: filepath.Join(s.m.dir, ".process-scopes")}
	env, err := s.processes.Register(id, 552, "worker")
	if err != nil {
		t.Fatal(err)
	}
	path := archiveServiceFixture(t, filepath.Join(s.m.dir, "tasks", task.ID, "i552"), env)
	// Ownership must survive recreating the registry (application restart).
	s.processes = &taskproc.Registry{Root: s.processes.Root}
	w := httptest.NewRecorder()
	s.estimateTaskArchives(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"task_ids":["`+task.ID+`"]}`)))
	var preview archiveSpaceEstimate
	if err = json.Unmarshal(w.Body.Bytes(), &preview); err != nil || len(preview.Writers) != 1 || !preview.Writers[0].Managed || len(preview.BackgroundProcesses) != 1 {
		t.Fatalf("preview=%s err=%v", w.Body.String(), err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "ready\n" {
		t.Fatal("preview stopped service")
	}
	job, err := s.prepareTaskArchive(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	archived := filepath.Join(job.PreparedPath, "files", "tasks", task.ID, "i552", "background.log")
	if raw, err := os.ReadFile(archived); err != nil || string(raw) != "ready\nshutdown-final\n" {
		t.Fatalf("shutdown log lost: %q %v", raw, err)
	}
	if _, err = s.m.pg.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = s.archivePreparedTask(job); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveCapacityAndCopyFailuresKeepOriginalAfterServiceShutdown(t *testing.T) {
	for _, failure := range []string{"before_stop", "after_stop", "copy"} {
		t.Run(failure, func(t *testing.T) {
			s, task, id := directoryArchiveFixture(t)
			s.processes = &taskproc.Registry{Root: filepath.Join(s.m.dir, ".process-scopes")}
			env, err := s.processes.Register(id, 562, "worker")
			if err != nil {
				t.Fatal(err)
			}
			path := archiveServiceFixture(t, filepath.Join(s.m.dir, "tasks", task.ID, "i562"), env)
			queries := 0
			s.archiveSpaceQuery = func(string) (uint64, uint64, error) {
				queries++
				if failure == "before_stop" || failure == "after_stop" && queries > 1 {
					return 0, 4096, nil
				}
				return 1 << 40, 4096, nil
			}
			if failure == "copy" {
				s.archiveCopySource = func(context.Context, archiveSource, string) error { return errors.New("fixture copy failed") }
			}
			if _, err = s.prepareTaskArchive(t.Context(), id); err == nil {
				t.Fatal("failure was ignored")
			}
			want := "ready\nshutdown-final\n"
			if failure == "before_stop" {
				want = "ready\n"
			}
			if raw, e := os.ReadFile(path); e != nil || string(raw) != want {
				t.Fatalf("original data=%q err=%v", raw, e)
			}
			var archived bool
			if err := s.m.pg.QueryRow(`SELECT archived_at IS NOT NULL FROM tasks WHERE id=$1`, id).Scan(&archived); err != nil || archived {
				t.Fatalf("task archived on failure: %t %v", archived, err)
			}
			job, err := s.m.pg.GetTaskArchiveByTask(id)
			if err != nil {
				t.Fatal(err)
			}
			if job != nil && job.State != "archive_failed" {
				t.Fatalf("invalid archive state: %+v", job)
			}
			if s.engine.IsDeleting(task.ID) {
				t.Fatal("barrier retained on failure")
			}
		})
	}
}
