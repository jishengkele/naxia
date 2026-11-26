//go:build linux

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jishengkele/naxia/taskproc"
)

func TestDeleteTaskStopsServicesEvenWhenKeepingFiles(t *testing.T) {
	for _, removeFiles := range []bool{false, true} {
		t.Run(fmt.Sprint(removeFiles), func(t *testing.T) {
			s, task, id := directoryArchiveFixture(t)
			registry := &taskproc.Registry{Root: filepath.Join(s.m.dir, ".process-scopes")}
			env, err := registry.Register(id, 1, "worker")
			if err != nil {
				t.Fatal(err)
			}
			log := archiveServiceFixture(t, filepath.Join(s.m.dir, "tasks", task.ID, "background"), env)
			foreignEnv, err := registry.Register(id+1000, 2, "foreign")
			if err != nil {
				t.Fatal(err)
			}
			foreign := archiveServiceFixture(t, t.TempDir(), foreignEnv)
			w := httptest.NewRecorder()
			r := httptest.NewRequest("DELETE", "/", strings.NewReader(fmt.Sprintf(`{"delete_files":%t}`, removeFiles)))
			r.SetPathValue("id", task.ID)
			s.pgDeleteTask(w, r)
			if w.Code != 200 {
				t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
			}
			if row, err := s.m.pg.GetTask(id); err != nil || row != nil {
				t.Fatalf("task not deleted: %+v %v", row, err)
			}
			if owned, err := registry.List(id); err != nil || len(owned) != 0 {
				t.Fatalf("services remain: %+v %v", owned, err)
			}
			if removeFiles {
				if _, err := os.Stat(log); !os.IsNotExist(err) {
					t.Fatalf("files kept despite option: %v", err)
				}
			} else {
				if data, err := os.ReadFile(log); err != nil || string(data) != "ready\nshutdown-final\n" {
					t.Fatalf("shutdown write lost: %q %v", data, err)
				}
			}
			if data, _ := os.ReadFile(foreign); string(data) != "ready\n" {
				t.Fatal("foreign service affected")
			}
		})
	}
}

func TestDeleteTaskStopsUnmarkedBrowserChildOnlyWhenExclusive(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared=%t", shared), func(t *testing.T) {
			s, task, id := directoryArchiveFixture(t)
			root := filepath.Join(s.m.dir, "tasks", task.ID)
			registry := &taskproc.Registry{Root: filepath.Join(s.m.dir, ".process-scopes")}
			env, err := registry.Register(id, 2148, "browser")
			if err != nil {
				t.Fatal(err)
			}
			foreignTask := ""
			if shared {
				foreignTask = fmt.Sprint(id + 1000)
				foreignRoot := filepath.Join(s.m.dir, "tasks", foreignTask)
				if err := os.MkdirAll(foreignRoot, 0700); err != nil {
					t.Fatal(err)
				}
				foreignFile := filepath.Join(foreignRoot, "shared-profile")
				if err := os.WriteFile(foreignFile, []byte("shared"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			log := filepath.Join(root, "browser.log")
			cmd := exec.Command("python3", "-c", `
import os, subprocess, sys
child = """
import os, pathlib, signal, sys, time
f = open(sys.argv[1], "w")
foreign = (pathlib.Path.cwd().parent / sys.argv[2] / "shared-profile").open("rb") if sys.argv[2] else None
def stop(*args):
    f.write("shutdown-final\\n")
    f.close()
    os._exit(0)
signal.signal(signal.SIGTERM, stop)
f.write("ready\\n")
f.flush()
while True: time.sleep(.05)
"""
env = dict(os.environ)
env.pop("NAXIA_PROCESS_SCOPE", None)
p = subprocess.Popen([sys.executable, "-c", child, *sys.argv[1:]], env=env)
print(p.pid, flush=True)
p.wait()
`, log, foreignTask)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), env...)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
			var pid int
			if _, err := fmt.Fscan(stdout, &pid); err != nil {
				t.Fatal(err)
			}
			child, err := os.FindProcess(pid)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = child.Kill(); _ = child.Release() })
			deadline := time.Now().Add(5 * time.Second)
			for {
				if data, _ := os.ReadFile(log); string(data) == "ready\n" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("browser child not ready")
				}
				time.Sleep(10 * time.Millisecond)
			}
			w := httptest.NewRecorder()
			req := httptest.NewRequest("DELETE", "/", strings.NewReader(`{"delete_files":false}`))
			req.SetPathValue("id", task.ID)
			s.pgDeleteTask(w, req)
			row, err := s.m.pg.GetTask(id)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if shared {
				if w.Code != http.StatusConflict || row == nil || string(data) != "ready\n" ||
					!strings.Contains(w.Body.String(), "task_delete_processes") {
					t.Fatalf("shared child affected: status=%d body=%s log=%q", w.Code, w.Body.String(), data)
				}
				if owned, err := registry.List(id); err != nil || len(owned) != 1 {
					t.Fatalf("registered launcher affected: %+v %v", owned, err)
				}
			} else if w.Code != http.StatusOK || row != nil || string(data) != "ready\nshutdown-final\n" {
				t.Fatalf("exclusive child not closed before deletion: status=%d body=%s log=%q", w.Code, w.Body.String(), data)
			}
		})
	}
}

// No file in the task is open. The cwd alone proves that deletion would leave
// a live browser/service behind, but does not authorize killing it.
func TestDeleteUnknownIdleProcessReturnsConflictWithoutDeleting(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	root := filepath.Join(s.m.dir, "tasks", task.ID)
	cmd := exec.Command("python3", "-c", "import time; time.sleep(60)")
	cmd.Dir = root
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/", strings.NewReader(`{"delete_files":true}`))
	r.SetPathValue("id", task.ID)
	s.pgDeleteTask(w, r)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "task_delete_processes") || !strings.Contains(w.Body.String(), "cwd:") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if row, err := s.m.pg.GetTask(id); err != nil || row == nil {
		t.Fatalf("task lost: %+v %v", row, err)
	}
	if _, err := os.Stat(filepath.Join(root, "workspace", "binary.bin")); err != nil {
		t.Fatal("original files lost", err)
	}
	if s.engine.IsDeleting(task.ID) {
		t.Fatal("failed deletion retained barrier")
	}
}

func TestDeleteTaskServiceTimeoutKeepsFilesAndDatabase(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	registry := &taskproc.Registry{Root: filepath.Join(s.m.dir, ".process-scopes")}
	env, err := registry.Register(id, 1, "ignore-term")
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command("python3", "-c", `import signal,time,sys
signal.signal(signal.SIGTERM,signal.SIG_IGN)
open(sys.argv[1],'w').write('ready')
while True:time.sleep(.05)
`, ready)
	cmd.Dir = filepath.Join(s.m.dir, "tasks", task.ID)
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("service not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	_, err = s.m.DeleteTaskContext(ctx, task.ID, DeleteTaskOptions{DeleteFiles: true})
	var conflict *taskDeleteProcessConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("expected service conflict: %v", err)
	}
	if owned, err := registry.List(id); err != nil || len(owned) != 1 {
		t.Fatalf("service was force killed: %+v %v", owned, err)
	}
	if row, err := s.m.pg.GetTask(id); err != nil || row == nil {
		t.Fatalf("task lost: %+v %v", row, err)
	}
	if _, err := os.Stat(filepath.Join(s.m.dir, "tasks", task.ID, "workspace", "binary.bin")); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteTaskFailureRestoresFilesAfterServiceShutdown(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	registry := &taskproc.Registry{Root: filepath.Join(s.m.dir, ".process-scopes")}
	env, err := registry.Register(id, 1, "worker")
	if err != nil {
		t.Fatal(err)
	}
	log := archiveServiceFixture(t, filepath.Join(s.m.dir, "tasks", task.ID, "background"), env)
	name := fmt.Sprintf("test_process_delete_%d", id)
	if _, err = s.m.pg.Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN RAISE EXCEPTION 'fixture process delete failure'; END $body$`, name)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.m.pg.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON tasks`, name))
		_, _ = s.m.pg.Exec(fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
	})
	if _, err = s.m.pg.Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE DELETE ON tasks FOR EACH ROW WHEN (OLD.id=%d) EXECUTE FUNCTION %s()`, name, id, name)); err != nil {
		t.Fatal(err)
	}
	_, err = s.m.DeleteTask(task.ID, DeleteTaskOptions{DeleteFiles: true})
	if err == nil || !strings.Contains(err.Error(), "fixture process delete failure") || !strings.Contains(err.Error(), "不会自动重启") {
		t.Fatalf("unexpected failure: %v", err)
	}
	if data, err := os.ReadFile(log); err != nil || string(data) != "ready\nshutdown-final\n" {
		t.Fatalf("rollback lost shutdown log: %q %v", data, err)
	}
	if row, err := s.m.pg.GetTask(id); err != nil || row == nil {
		t.Fatalf("task row lost: %+v %v", row, err)
	}
}

func TestDeleteChecksRenamedReferencesAndLateNonWritingServices(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	root := filepath.Join(s.m.dir, "tasks", task.ID)
	stage, err := stageTaskFiles(s.m.dir, task.ID, task.ExpID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stage.rollback() })
	for _, move := range stage.moves {
		if move.source == root {
			root = move.staged
			break
		}
	}
	registry := &taskproc.Registry{Root: filepath.Join(s.m.dir, ".process-scopes")}
	file, err := os.Open(filepath.Join(root, "workspace", "binary.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureTaskDeleteProcessesQuiet(t.Context(), registry, id, []string{root}); err == nil {
		t.Fatal("read-only staged descriptor ignored")
	}
	_ = file.Close()
	env, err := registry.Register(id, 1, "late")
	if err != nil {
		t.Fatal(err)
	}
	archiveServiceFixture(t, t.TempDir(), env)
	if err := ensureTaskDeleteProcessesQuiet(t.Context(), registry, id, []string{root}); err == nil || !strings.Contains(err.Error(), "仍存活") {
		t.Fatalf("late service ignored: %v", err)
	}
}
