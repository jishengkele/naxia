//go:build linux

package taskproc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The launching shell/Python exits immediately, reproducing both '&' and nohup.
// The child changes session, keeps a writable file open, and flushes on TERM.
func detachedFixture(t *testing.T, env []string, ignore bool) (int, string) {
	t.Helper()
	root := t.TempDir()
	code := `import os,sys,signal,time
pid=os.fork()
if pid:
 print(pid,flush=True)
 os._exit(0)
os.setsid()
dev=os.open('/dev/null',os.O_RDWR)
for fd in (0,1,2):os.dup2(dev,fd)
f=open(sys.argv[1],'a')
def finish(*args):
 f.write('shutdown-final\n');f.flush();os.fsync(f.fileno());f.close();os._exit(0)
signal.signal(signal.SIGTERM,signal.SIG_IGN if sys.argv[2]=='ignore' else finish)
f.write('ready\n');f.flush()
while True:time.sleep(.05)
`
	mode := "normal"
	if ignore {
		mode = "ignore"
	}
	path := filepath.Join(root, "service.log")
	cmd := exec.Command("python3", "-c", code, path, mode)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("kill", "-KILL", strconv.Itoa(pid)).Run() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := os.ReadFile(path); strings.Contains(string(b), "ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return pid, path
}

func TestDetachedProcessesRemainOwnedAcrossRegistryRestartAndFlush(t *testing.T) {
	r := &Registry{Root: t.TempDir()}
	env, err := r.Register(6, 552, "worker-552")
	if err != nil {
		t.Fatal(err)
	}
	pid, path := detachedFixture(t, env, false)
	// Another task and a legacy unregistered process must survive.
	foreignEnv, err := r.Register(7, 1, "other")
	if err != nil {
		t.Fatal(err)
	}
	foreign, _ := detachedFixture(t, foreignEnv, false)
	unknown, _ := detachedFixture(t, nil, false)
	r = &Registry{Root: r.Root}
	list, err := r.List(6)
	if err != nil || len(list) != 1 || list[0].PID != pid || list[0].IntentID != 552 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	if err := r.Stop(t.Context(), 6); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "ready\nshutdown-final\n" {
		t.Fatalf("shutdown data=%q err=%v", raw, err)
	}
	if list, err = r.List(6); err != nil || len(list) != 0 {
		t.Fatalf("remaining=%+v err=%v", list, err)
	}
	for _, pid := range []int{foreign, unknown} {
		_, state, err := identity(pid)
		if err != nil || state == "Z" {
			t.Fatalf("unrelated process stopped: %d %s %v", pid, state, err)
		}
	}
}

func TestIdentityChangeAndStopTimeoutNeverForceKill(t *testing.T) {
	r := &Registry{Root: t.TempDir()}
	env, err := r.Register(6, 562, "worker-562")
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := detachedFixture(t, env, true)
	list, err := r.List(6)
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	changed := list[0]
	changed.StartTicks++
	if err = signalTerm(changed); err == nil {
		t.Fatal("changed identity was accepted")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	if err = r.Stop(ctx, 6); err == nil {
		t.Fatal("ignoring TERM reported as stopped")
	}
	_, state, err := identity(pid)
	if err != nil || state == "Z" {
		t.Fatalf("force killed service: %s %v", state, err)
	}
}

func TestCorruptOwnershipRecordFailsClosed(t *testing.T) {
	r := &Registry{Root: t.TempDir()}
	if _, err := r.Register(6, 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Root, "6", "invalid.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.List(6); err == nil {
		t.Fatal("invalid registry silently ignored")
	}
}
