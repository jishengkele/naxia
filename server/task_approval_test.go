package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

type taskApprovalProvider struct {
	input json.RawMessage
	id    string
	calls atomic.Int64
}

func (p *taskApprovalProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	if p.calls.Add(1) == 1 {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: p.id, Name: "Write", Input: p.input}}}, "tool_use", llm.Usage{}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("approval run complete")}}, "end_turn", llm.Usage{}, nil
}

func (p *taskApprovalProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		msg, stop, _, _ := p.Complete(ctx, req)
		events := []llm.StreamEvent{}
		for _, block := range msg.Content {
			if block.Type == llm.BlockToolUse {
				events = append(events, llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: block.ID, ToolName: block.Name}, llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(block.Input)})
			} else {
				events = append(events, llm.StreamEvent{Type: llm.SETextDelta, Text: block.Text})
			}
		}
		events = append(events, llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop}, llm.StreamEvent{Type: llm.SEMessageStop})
		for _, event := range events {
			if !yield(event, nil) {
				return
			}
		}
	}
}

// Go through the production task bundle, normal/final engine entry points and
// authenticated HTTP approval API. No server.New schedulers or external model run.
func TestTaskApprovalServerWiringAndSessionDisplay(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	t.Setenv("AGENT_CORE_DISABLE_BACKGROUND_TASKS", "1")
	oldTools, err := m.interceptor.GetEnabledTools()
	if err != nil {
		t.Fatal(err)
	}
	oldConfig := m.interceptor.GetJudgeConfig()
	t.Cleanup(func() { _ = m.interceptor.SetEnabledTools(oldTools); _ = m.interceptor.SetJudgeConfig(oldConfig) })
	if err := m.interceptor.SetEnabledTools([]string{"Write"}); err != nil {
		t.Fatal(err)
	}
	rule, err := m.pg.CreateInterceptRule("task HTTP approval regression", "tool_name", "regex", "^Write$", "ask", "approve marker", 100000, true, true, 10, "deny")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec(`DELETE FROM intercept_rules WHERE id=$1`, rule.ID) })
	m.interceptor.Invalidate()
	for _, role := range []string{"planner", "planner_final", "main:0", "main:3"} {
		for _, allow := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/allow_%t", role, allow), func(t *testing.T) {
				task, err := m.CreateTask("HTTP approval fixture", "local marker only", nil, 0, 600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := task.Store.AddGoal(map[string]any{"text": "fixture remains open"}, "human"); err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				marker := filepath.Join(dir, "marker.txt")
				input, _ := json.Marshal(map[string]string{"file_path": marker, "content": "approved"})
				provider := &taskApprovalProvider{input: input, id: "server-approval-" + task.ID}
				ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
				s := &Server{m: m, ctx: ctx, engine: NewEngine(m), jwtKey: []byte("task-approval-http-regression-key"),
					taskAgents: map[string]*taskAgentBundle{}, chatBusy: map[string]bool{}, chatCancel: map[string]context.CancelCauseFunc{},
					llmOn: true, llmProv: provider, llmCfg: agent.Config{Stream: true}}
				s.engine.SetAuthoritativeAgentResolver(func(t *Task) (*agent.Planner, *agent.Worker) { b := s.agentsForTask(t); return b.pl, b.wk })
				s.engine.SetReadiness(func() bool { return true })
				t.Cleanup(func() {
					cancel()
					s.engine.StopTask(task.ID)
					_, _ = m.pg.Exec(`DELETE FROM intercept_pending WHERE task_id=$1`, task.ID)
					_, _ = m.pg.Exec(`DELETE FROM tasks WHERE id=$1`, task.ID)
					_, _ = m.pg.Exec(`DELETE FROM explorations WHERE id=$1`, task.ExpID)
				})
				token, err := signJWT(s.jwtKey)
				if err != nil {
					t.Fatal(err)
				}
				handler := s.Handler()
				streamServer := httptest.NewServer(handler)
				defer streamServer.Close()
				streamRequest, err := http.NewRequestWithContext(ctx, "GET", streamServer.URL+"/api/exploration/activity/stream?task="+task.ID, nil)
				if err != nil {
					t.Fatal(err)
				}
				streamRequest.Header.Set("Authorization", "Bearer "+token)
				stream, err := streamServer.Client().Do(streamRequest)
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Body.Close()
				if stream.StatusCode != http.StatusOK || stream.Header.Get("Content-Type") != "text/event-stream" {
					t.Fatalf("activity stream status=%d headers=%v", stream.StatusCode, stream.Header)
				}
				request := func(method, path, body string, auth bool) *httptest.ResponseRecorder {
					r := httptest.NewRequest(method, path, strings.NewReader(body))
					if auth {
						r.Header.Set("Authorization", "Bearer "+token)
					}
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, r)
					return w
				}
				wait := func(check func() bool) {
					t.Helper()
					for !check() {
						select {
						case <-ctx.Done():
							t.Fatal("timed out waiting for approval lifecycle")
						case <-time.After(10 * time.Millisecond):
						}
					}
				}
				var finalDone chan struct{}
				session, agentName := "plan", "planner"
				if strings.HasPrefix(role, "main:") {
					session, agentName = role, "mainagent"
					seg := 0
					if role == "main:3" {
						seg = 3
						for range seg {
							if _, err := task.Store.NewMainSession(); err != nil {
								t.Fatal(err)
							}
						}
					}
					// A forged segment must not strand approvals outside the session list.
					if got := request("POST", "/api/chat?task="+task.ID, `{"message":"write marker","seg":999}`, true); got.Code != 404 {
						t.Fatalf("nonexistent main segment=%d %s", got.Code, got.Body.String())
					}
					body := fmt.Sprintf(`{"message":"write marker","seg":%d}`, seg)
					if got := request("POST", "/api/chat?task="+task.ID, body, false); got.Code != 401 {
						t.Fatalf("chat auth=%d", got.Code)
					}
					if got := request("POST", "/api/chat?task="+task.ID, body, true); got.Code != 202 {
						t.Fatalf("chat=%d %s", got.Code, got.Body.String())
					}
				} else if role == "planner_final" {
					finalDone = make(chan struct{})
					go func() { defer close(finalDone); s.engine.runFinalPlannerRound(ctx, task) }()
				} else {
					s.engine.Run(ctx, task)
				}
				var pending db.InterceptApprovalRow
				wait(func() bool {
					got := request("GET", "/api/intercept/task/"+task.ID, "", true)
					var payload struct {
						Items []db.InterceptApprovalRow `json:"items"`
					}
					if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &payload) != nil || len(payload.Items) != 1 {
						return false
					}
					pending = payload.Items[0]
					return pending.Status == "pending"
				})
				if pending.TaskID == nil || *pending.TaskID != task.ID || pending.AgentName != agentName || pending.Session != session {
					t.Fatalf("wrong pending scope: %+v", pending)
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("write ran before approval")
				}
				var inline ActivityDTO
				wait(func() bool {
					got := request("GET", "/api/exploration/activity/history?task="+task.ID+"&session="+session, "", true)
					var payload struct {
						Items []ActivityDTO `json:"items"`
					}
					if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &payload) != nil {
						return false
					}
					for _, item := range payload.Items {
						if item.Kind == "intercept_request" {
							inline = item
							return true
						}
					}
					return false
				})
				if inline.Worker != agentName {
					t.Fatalf("wrong inline lane: %+v", inline)
				}
				// Verify the same approval is delivered live, not only after a history reload.
				scanner := bufio.NewScanner(stream.Body)
				var live ActivityDTO
				for scanner.Scan() {
					line := scanner.Text()
					if strings.HasPrefix(line, "data: ") {
						if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &live); err != nil {
							t.Fatal(err)
						}
						if live.Kind == "intercept_request" {
							break
						}
					}
				}
				if live.Kind != "intercept_request" || live.Seq != inline.Seq || live.Worker != inline.Worker || !sameTaskApprovalSegment(live.MainSeg, inline.MainSeg) {
					t.Fatalf("live approval=%+v history=%+v stream error=%v", live, inline, scanner.Err())
				}
				if session == "main:3" {
					got := request("GET", "/api/exploration/activity/history?task="+task.ID+"&session=main:0", "", true)
					if strings.Contains(got.Body.String(), "intercept_request") {
						t.Fatal("approval leaked into main:0")
					}
				}
				decisionPath := fmt.Sprintf("/api/intercept/pending/%d/decide", pending.ID)
				decision := "denied"
				if allow {
					decision = "allowed"
				}
				body := fmt.Sprintf(`{"decision":%q}`, decision)
				if got := request("POST", decisionPath, body, false); got.Code != 401 {
					t.Fatal("approval decision bypassed auth")
				}
				if got := request("POST", decisionPath, body, true); got.Code != 200 {
					t.Fatalf("decide=%d %s", got.Code, got.Body.String())
				}
				wait(func() bool {
					var n int
					err := m.pg.QueryRow(`SELECT count(*) FROM activity WHERE exploration_id=$1 AND worker=$2 AND kind='result'`, task.ExpID, agentName).Scan(&n)
					return err == nil && n > 0
				})
				_, statErr := os.Stat(marker)
				if (statErr == nil) != allow {
					t.Fatalf("executed=%t allow=%t", statErr == nil, allow)
				}
				got := request("GET", fmt.Sprintf("/api/intercept/history/%d/execution", pending.ID), "", true)
				var execution struct {
					Session string  `json:"session"`
					TaskID  *string `json:"task_id"`
				}
				if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &execution) != nil || execution.Session != session || execution.TaskID == nil || *execution.TaskID != task.ID {
					t.Fatalf("execution navigation=%d %s", got.Code, got.Body.String())
				}
				if finalDone != nil {
					select {
					case <-finalDone:
					case <-ctx.Done():
						t.Fatal("final planner did not finish")
					}
				}
				if agentName == "mainagent" {
					wait(func() bool { s.chatMu.Lock(); defer s.chatMu.Unlock(); return !s.chatBusy[task.ID] })
				}
			})
		}
	}
}

func sameTaskApprovalSegment(a, b *int) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func TestTaskChatMainSessionCompatibility(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	task, err := m.CreateTask("main session compatibility", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = m.pg.Exec(`DELETE FROM tasks WHERE id=$1`, task.ID)
		_, _ = m.pg.Exec(`DELETE FROM explorations WHERE id=$1`, task.ExpID)
	})
	for range 2 {
		if _, err := task.Store.NewMainSession(); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{m: m, ctx: t.Context(), engine: NewEngine(m), chatBusy: map[string]bool{}, chatCancel: map[string]context.CancelCauseFunc{}}
	for _, tc := range []struct {
		name string
		body string
		seg  int
		code int
	}{
		{"omitted uses latest", `{"message":"fixture"}`, 2, 200},
		{"legacy negative uses latest", `{"message":"fixture","seg":-1}`, 2, 200},
		{"original remains interactive", `{"message":"fixture","seg":0}`, 0, 200},
		{"older remains interactive", `{"message":"fixture","seg":1}`, 1, 200},
		{"missing segment rejected", `{"message":"fixture","seg":999}`, 999, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before int64
			if err := m.pg.QueryRow(`SELECT COALESCE(MAX(id),0) FROM activity WHERE exploration_id=$1`, task.ExpID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			s.chat(w, httptest.NewRequest("POST", "/api/chat?task="+task.ID, strings.NewReader(tc.body)))
			if w.Code != tc.code {
				t.Fatalf("chat=%d want=%d %s", w.Code, tc.code, w.Body.String())
			}
			items, _, err := task.Store.ActivityList(nil, before, 100)
			if err != nil {
				t.Fatal(err)
			}
			if tc.code != 200 && len(items) != 0 {
				t.Fatal("invalid segment persisted activity")
			}
			if tc.code == 200 && len(items) == 0 {
				t.Fatal("valid segment did not receive activity")
			}
			for _, item := range items {
				if item.Worker != "mainagent" || item.MainSeg == nil || *item.MainSeg != tc.seg {
					t.Fatalf("wrong segment activity: %+v", item)
				}
			}
			if s.chatBusy[task.ID] {
				t.Fatal("request retained the task chat busy flag")
			}
		})
	}
}
