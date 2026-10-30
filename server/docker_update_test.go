package server

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDockerWorkerUnavailableDoesNotFallback(t *testing.T) {
	t.Setenv("ARTEX_DOCKER_UPDATE_SOCKET", "")
	if _, err := dockerUpdateCall(t.Context(), "POST", "/apply", map[string]string{"version": "v1.0.3"}); err == nil {
		t.Fatal("unconfigured Docker updater must reject, never stage a binary")
	}
}

func TestDockerWorkerSocketProtocol(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket deployment")
	}
	socket := filepath.Join(t.TempDir(), "control.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/apply" {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["version"] != "v1.0.3" || len(body) != 1 {
				t.Error("unexpected worker request")
			}
			if r.URL.Path != "/apply" {
				t.Error("unexpected worker route")
			}
		}
		if r.URL.Path == "/reject" {
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"error":"busy"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ready":true,"running":true,"phase":"downloading","version":"v1.0.3"}`))
	}))
	_ = srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	defer srv.Close()
	t.Setenv("ARTEX_DOCKER_UPDATE_SOCKET", socket)
	st, err := dockerUpdateCall(t.Context(), "POST", "/apply", map[string]string{"version": "v1.0.3"})
	if err != nil || !st.Running || st.Version != "v1.0.3" {
		t.Fatalf("status=%+v err=%v", st, err)
	}
	if _, err := dockerUpdateCall(t.Context(), "POST", "/reject", nil); err == nil {
		t.Fatal("worker rejection lost")
	}
}
