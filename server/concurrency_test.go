package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func concurrencyTestServer(t *testing.T) *Server {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return &Server{m: m, ctx: t.Context(), engine: NewEngine(m), jwtKey: []byte("concurrency-regression-key"), provByProfile: map[int64]*provEntry{}}
}

func TestProfileConcurrencyHTTPAndRuntime(t *testing.T) {
	s := concurrencyTestServer(t)
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	created := request("POST", "/api/llm/profiles", `{"name":"concurrency-http","format":"openai","model":"fixture","api_key":"original-fixture-key","max_concurrency":3}`)
	var payload struct {
		ID int64 `json:"id"`
	}
	if created.Code != 200 || json.Unmarshal(created.Body.Bytes(), &payload) != nil || payload.ID == 0 {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, payload.ID) })
	for _, tc := range []struct {
		name, field, key string
		want             int
	}{
		{"update without key", `,"max_concurrency":2`, "", 2},
		{"legacy client omits field", "", "", 2},
		{"replace key", `,"max_concurrency":1`, "replacement-fixture-key", 1},
		{"disable limit", `,"max_concurrency":0`, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"id":%d,"name":"concurrency-http","format":"openai","model":"fixture","api_key":%q%s}`, payload.ID, tc.key, tc.field)
			got := request("POST", "/api/llm/profiles", body)
			if got.Code != 200 {
				t.Fatalf("save=%d %s", got.Code, got.Body.String())
			}
			cfg, ok := s.loadProfileConfig(payload.ID)
			if !ok || cfg.MaxConcurrency != tc.want {
				t.Fatalf("runtime config limit=%d ok=%t want=%d", cfg.MaxConcurrency, ok, tc.want)
			}
			_, cached, ok := s.providerForProfile(payload.ID)
			if !ok || cached.MaxConcurrency != tc.want {
				t.Fatal("cached provider missed limit")
			}
			listed := request("GET", "/api/llm/profiles", "")
			var list struct {
				Profiles []LLMProfileDTO `json:"profiles"`
			}
			if listed.Code != 200 || json.Unmarshal(listed.Body.Bytes(), &list) != nil {
				t.Fatal("profile list failed")
			}
			found := false
			for _, profile := range list.Profiles {
				if profile.ID == fmt.Sprint(payload.ID) {
					found = profile.MaxConcurrency == tc.want
				}
			}
			if !found {
				t.Fatal("DTO lost limit")
			}
		})
	}
	for _, value := range []string{"-1", "1.5", "2147483648"} {
		body := fmt.Sprintf(`{"id":%d,"name":"concurrency-http","format":"openai","model":"fixture","max_concurrency":%s}`, payload.ID, value)
		got := request("POST", "/api/llm/profiles", body)
		if got.Code != 400 {
			t.Fatalf("invalid limit %s status=%d %s", value, got.Code, got.Body.String())
		}
	}
	profile, err := s.m.pg.ProfileByID(payload.ID)
	if err != nil || profile == nil || profile.MaxConcurrency != 0 || profile.APIKey != "replacement-fixture-key" {
		t.Fatal("invalid save mutated persisted profile")
	}
}

func TestProfileConcurrencyEditWakesOldProviderWaiters(t *testing.T) {
	s := concurrencyTestServer(t)
	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	id, err := s.m.pg.SaveProfile(&db.LLMProfile{Name: "concurrency-live-edit", Format: "openai", BaseURL: upstream.URL, APIKey: "fixture", Model: "fixture", MaxConcurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	provider, _, ok := s.providerForProfile(id)
	if !ok {
		t.Fatal("provider unavailable")
	}
	done := make(chan error, 2)
	call := func() {
		_, _, _, err := provider.Complete(ctx, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("fixture")}})
		done <- err
	}
	go call()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go call()
	select {
	case <-entered:
		t.Fatal("second request exceeded old limit")
	case <-time.After(25 * time.Millisecond):
	}
	w := httptest.NewRecorder()
	body := fmt.Sprintf(`{"id":%d,"name":"concurrency-live-edit","format":"openai","base_url":%q,"model":"fixture","max_concurrency":2}`, id, upstream.URL)
	s.pgSaveProfile(w, httptest.NewRequest("POST", "/api/llm/profiles", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("edit=%d %s", w.Code, w.Body.String())
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("new limit did not wake an old provider's waiter")
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestEgressHTTPDoesNotLeakMalformedProxyCredentials(t *testing.T) {
	s := concurrencyTestServer(t)
	old := defaultEgressProbes
	defaultEgressProbes = []string{"http://127.0.0.1:1"} // blocked locally, no real external probe
	t.Cleanup(func() { defaultEgressProbes = old })
	for _, raw := range []string{"http://u:s3cr3t/p4ss@127.0.0.1:1", "http://u:123/privateTail@127.0.0.1:1", "//u:s3cr3t@127.0.0.1:1"} {
		body, _ := json.Marshal(map[string]string{"global_proxy": raw})
		w := httptest.NewRecorder()
		s.testEgressProxy(w, httptest.NewRequest("POST", "/api/settings/proxy/test", strings.NewReader(string(body))))
		if w.Code != 200 {
			t.Fatalf("probe=%d %s", w.Code, w.Body.String())
		}
		for _, secret := range []string{"s3cr3t", "p4ss", "123", "privateTail"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("HTTP response leaked %s: %s", secret, w.Body.String())
			}
		}
	}
}

func TestProfileConcurrencyLegacyEndpointPreservesTarget(t *testing.T) {
	s := concurrencyTestServer(t)
	oldActive, err := s.m.pg.ActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := s.m.pg.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	var oldDefault *db.LLMProfile
	for _, p := range profiles {
		if p.Name == "default" {
			oldDefault, err = s.m.pg.ProfileByID(p.ID)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	target := &db.LLMProfile{Name: "default", Format: "openai", Model: "fixture", APIKey: "fixture", MaxConcurrency: 4, Streaming: false, MaxTokens: 77, SessionHeaderKey: "x-fixture-session", Retry: db.RetryOverride{Connect: db.RetryRule{Attempts: 2}}}
	if oldDefault != nil {
		target.ID = oldDefault.ID
	}
	targetID, err := s.m.pg.SaveProfile(target)
	if err != nil {
		t.Fatal(err)
	}
	var activeID int64
	t.Cleanup(func() {
		if oldDefault != nil {
			_, _ = s.m.pg.SaveProfile(oldDefault)
		} else {
			_, _ = s.m.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, targetID)
		}
		if activeID != 0 {
			_, _ = s.m.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, activeID)
		}
		if oldActive != nil {
			_ = s.m.pg.SetActiveProfile(oldActive.ID)
		} else {
			_, _ = s.m.pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE is_default`)
		}
	})
	activeID, err = s.m.pg.SaveProfile(&db.LLMProfile{Name: "concurrency-other-active", Format: "openai", Model: "fixture", APIKey: "fixture", MaxConcurrency: 2, Streaming: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.SetActiveProfile(activeID); err != nil {
		t.Fatal(err)
	}
	cfg, ok := s.loadProfileConfig(activeID)
	if !ok {
		t.Fatal("profile config unavailable")
	}
	if err := s.applyLLM(cfg); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		field string
		want  int
	}{{"", 4}, {`,"max_concurrency":1`, 1}, {"", 1}, {`,"max_concurrency":0`, 0}} {
		w := httptest.NewRecorder()
		s.setLLM(w, httptest.NewRequest("POST", "/api/llm", strings.NewReader(`{"provider":"openai","model":"fixture"`+tc.field+`}`)))
		if w.Code != 200 {
			t.Fatalf("save=%d %s", w.Code, w.Body.String())
		}
		var response struct {
			MaxConcurrency int `json:"max_concurrency"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.MaxConcurrency != tc.want {
			t.Fatalf("legacy response=%s err=%v", w.Body.String(), err)
		}
		p, err := s.m.pg.ProfileByID(targetID)
		if err != nil || p == nil || p.MaxConcurrency != tc.want {
			t.Fatalf("target limit lost: profile=%+v err=%v", p, err)
		}
		cached, cfg, ok := s.providerForProfile(targetID)
		if !ok || s.llmCfg != cfg || s.llmDirect != cached {
			t.Fatal("legacy global provider does not share the persisted profile's configuration and quotas")
		}
		if cfg.Stream || cfg.MaxTokens != 77 || cfg.SessionHeaderKey != "x-fixture-session" || cfg.Retry.ConnectAttempts != 2 {
			t.Fatalf("legacy endpoint reset persisted runtime options: %+v", cfg)
		}
	}
}
