package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/guard"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

// Exercise the actual role sessions and real Write tool, with persisted policy,
// approval records and activity. The provider is deterministic and never calls a model.
func TestTaskAgentApprovalLifecycle(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })
	oldAugment, oldResolve := ToolAugment, ToolResolve
	ToolAugment, ToolResolve = nil, nil
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	t.Setenv("AGENT_CORE_DISABLE_BACKGROUND_TASKS", "1")
	ic := intercept.New(d)
	oldTools, err := ic.GetEnabledTools()
	if err != nil {
		t.Fatal(err)
	}
	oldConfig := ic.GetJudgeConfig()
	t.Cleanup(func() { _ = ic.SetEnabledTools(oldTools); _ = ic.SetJudgeConfig(oldConfig) })
	for _, role := range []string{"planner", "planner_final", "mainagent"} {
		for _, syncMode := range []bool{false, true} {
			for _, scenario := range []string{"deny", "allow", "ask_allow", "ask_deny", "timeout_allow", "timeout_deny", "model_deny", "model_ask", "model_allow", "model_fail_allow", "model_fail_deny", "model_fail_ask", "disabled", "judge_off", "cancel", "ask_allow_cancel", "model_fail_allow_cancel", "settlement_deny", "settlement_ask", "deferred_deny", "deferred_ask", "deferred_allow", "deferred_disabled", "deferred_both"} {
				t.Run(fmt.Sprintf("%s/sync_%t/%s", role, syncMode, scenario), func(t *testing.T) {
					task, err := d.CreateTask("approval regression", "local marker only", nil, 0, 600)
					if err != nil {
						t.Fatal(err)
					}
					taskID := strconv.FormatInt(task.ID, 10)
					t.Cleanup(func() {
						_, _ = d.Exec(`DELETE FROM intercept_pending WHERE task_id=$1`, taskID)
						_, _ = d.Exec(`DELETE FROM tasks WHERE id=$1`, task.ID)
						_, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, task.ExplorationID)
					})
					timeout := scenario == "timeout_allow" || scenario == "timeout_deny"
					action := "deny"
					if scenario == "allow" || scenario == "deferred_allow" {
						action = "allow"
					}
					if timeout || scenario == "ask_allow" || scenario == "ask_deny" || scenario == "cancel" || scenario == "ask_allow_cancel" || scenario == "settlement_ask" || scenario == "deferred_ask" {
						action = "ask"
					}
					fallback := strings.HasPrefix(scenario, "model_") || scenario == "judge_off"
					deferred := strings.HasPrefix(scenario, "deferred_")
					reviewedTool := "Write"
					if deferred {
						reviewedTool = "DeferredWriteProbe"
						write := actool.NewWrite()
						ToolAugment = func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
							probe := actool.Build(actool.Spec{Name: reviewedTool, Schema: write.InputSchema(), Run: write.Call})
							return []actool.CoreTool{probe}, DeferredInfo{Deferred: []string{reviewedTool}, GlobalNames: []string{reviewedTool}, Unlock: actool.NewUnlockSet(reviewedTool)}, nil
						}
						t.Cleanup(func() { ToolAugment = nil })
					}
					rule, err := d.CreateInterceptRule("task role regression", "tool_name", "regex", "^"+reviewedTool+"$", action, "regression policy", 100000, !fallback, timeout, 1, map[bool]string{true: "allow", false: "deny"}[scenario == "timeout_allow"])
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM intercept_rules WHERE id=$1`, rule.ID) })
					if scenario == "deferred_both" {
						outerRule, err := d.CreateInterceptRule("outer wrapper regression", "tool_name", "regex", "^ExecuteExtraTool$", "allow", "allow wrapper only", 100001, true, false, 0, "deny")
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM intercept_rules WHERE id=$1`, outerRule.ID) })
					}
					ic.Invalidate()
					enabled := []string{reviewedTool}
					if scenario == "disabled" || scenario == "deferred_disabled" {
						enabled = []string{}
					}
					if scenario == "deferred_both" {
						enabled = append(enabled, actool.ExecuteExtraToolName)
					}
					if err := ic.SetEnabledTools(enabled); err != nil {
						t.Fatal(err)
					}
					failAction := "deny"
					if strings.HasPrefix(scenario, "model_fail_") {
						failAction = strings.TrimSuffix(strings.TrimPrefix(scenario, "model_fail_"), "_cancel")
					}
					if err := ic.SetJudgeConfig(intercept.JudgeConfig{Enabled: fallback && scenario != "judge_off", TimeoutSeconds: 3, AskTimeoutSeconds: 2, AskTimeoutAction: "deny", FailAction: failAction, RetryCount: 0}); err != nil {
						t.Fatal(err)
					}
					judgeCalls := 0
					var cancelRun context.CancelFunc
					ic.SetReviewer(func(context.Context, int64, string, intercept.ReviewInput) (intercept.Decision, error) {
						judgeCalls++
						if scenario == "model_fail_allow_cancel" {
							cancelRun()
						}
						if strings.HasPrefix(scenario, "model_fail_") {
							return intercept.Decision{}, errors.New("fixture model unavailable")
						}
						verdict := "deny"
						if scenario == "model_ask" {
							verdict = "ask"
						}
						if scenario == "model_allow" {
							verdict = "allow"
						}
						return intercept.Decision{Action: verdict, Message: "regression model decision"}, nil
					})
					dir := t.TempDir()
					marker := filepath.Join(dir, "marker.txt")
					input, _ := json.Marshal(map[string]string{"file_path": marker, "content": "approved"})
					invokedTool, invocationInput := "Write", input
					if deferred {
						invokedTool = actool.ExecuteExtraToolName
						invocationInput, _ = json.Marshal(map[string]any{"tool_name": reviewedTool, "params": json.RawMessage(input)})
					}
					callID := "task-approval-" + taskID
					settlement := strings.HasPrefix(scenario, "settlement_")
					readInput, _ := json.Marshal(map[string]string{"file_path": filepath.Join(dir, "fixture.txt")})
					if settlement {
						if err := os.WriteFile(filepath.Join(dir, "fixture.txt"), []byte("fixture"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					turn := 0
					provider := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
						turn++
						events := []llm.StreamEvent{{Type: llm.SETextDelta, Text: "finished"}, {Type: llm.SEMessageDelta, StopReason: "end_turn"}, {Type: llm.SEMessageStop}}
						if turn == 1 && !settlement || turn == 2 && settlement {
							events = []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: callID, ToolName: invokedTool}, {Type: llm.SEToolInputJSON, Text: string(invocationInput)}, {Type: llm.SEMessageDelta, StopReason: "tool_use"}, {Type: llm.SEMessageStop}}
						}
						if turn == 1 && settlement {
							events = []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: callID + "-read", ToolName: "Read"}, {Type: llm.SEToolInputJSON, Text: string(readInput)}, {Type: llm.SEMessageDelta, StopReason: "tool_use"}, {Type: llm.SEMessageStop}}
						}
						for _, event := range events {
							if !yield(event, nil) {
								return
							}
						}
					}}
					ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
					cancelRun = cancel
					defer cancel()
					store := d.Exploration(task.ExplorationID)
					var mu sync.Mutex
					var activities []db.Activity
					emit := func(a db.Activity) {
						mu.Lock()
						activities = append(activities, a)
						mu.Unlock()
						if _, err := store.AppendActivity(a); err != nil {
							t.Errorf("persist activity: %v", err)
							cancel()
							return
						}
						if a.Kind != "intercept_request" {
							return
						}
						if _, err := os.Stat(marker); !os.IsNotExist(err) {
							t.Error("tool executed before approval")
						}
						var detail struct {
							ID int64 `json:"pending_id"`
						}
						if err := json.Unmarshal([]byte(a.Detail), &detail); err != nil {
							t.Error(err)
							cancel()
							return
						}
						if scenario == "cancel" {
							cancel()
							return
						}
						if !timeout {
							if err := ic.Decide(detail.ID, scenario == "ask_allow" || scenario == "model_ask" || scenario == "model_fail_ask" || scenario == "ask_allow_cancel"); err != nil {
								t.Error(err)
								cancel()
							}
							if scenario == "ask_allow_cancel" {
								cancel()
							}
						}
					}
					g := guard.NewWithInterceptor(ic)
					maxTurns := 3
					if settlement {
						maxTurns = 1 // the Write call must occur in the SDK's budget settlement phase
					}
					segment, session, agentName := 3, "plan", "planner"
					if role == "mainagent" {
						session, agentName = "main:3", "mainagent"
						m := NewMainAgent(provider, "fixture", dir, nil, 200000, maxTurns)
						m.SetGuard(g)
						m.SetNonStreaming(func() bool { return syncMode })
						_, err = m.Chat(ctx, task.ID, segment, nil, store, task.Goal, "write local marker", emit, nil, nil, nil, nil)
					} else {
						p := NewPlanner(provider, "fixture", dir, nil, 200000, maxTurns)
						p.SetGuard(g)
						p.SetNonStreaming(func() bool { return syncMode })
						if role == "planner_final" {
							ctx = WithTaskClock(ctx, TaskClock{Final: true})
						}
						_, _, err = p.Plan(ctx, task.ID, nil, store, task.Goal, nil, emit)
					}
					if err != nil && !((scenario == "cancel" || strings.HasSuffix(scenario, "_cancel")) && errors.Is(err, context.Canceled)) {
						t.Fatal(err)
					}
					if scenario == "cancel" || strings.HasSuffix(scenario, "_cancel") {
						// SDK cancellation can return a synthetic result before the hook
						// drains. Wait for its block/audit work before inspecting persistence.
						deadline := time.Now().Add(3 * time.Second)
						for {
							blocked := false
							for _, entry := range g.Audit() {
								blocked = blocked || entry.Tool == "Write" && entry.Action == "block"
							}
							if blocked {
								break
							}
							if time.Now().After(deadline) {
								t.Fatal("cancelled pre-tool hook did not drain")
							}
							time.Sleep(time.Millisecond)
						}
					}
					wantExecution := scenario == "allow" || scenario == "ask_allow" || scenario == "timeout_allow" || scenario == "model_ask" || scenario == "model_allow" || scenario == "model_fail_allow" || scenario == "model_fail_ask" || scenario == "disabled" || scenario == "judge_off" || scenario == "deferred_allow" || scenario == "deferred_disabled"
					_, statErr := os.Stat(marker)
					if (statErr == nil) != wantExecution {
						t.Fatalf("executed=%t want=%t", statErr == nil, wantExecution)
					}
					rows, err := d.ListTaskIntercepts(taskID)
					if err != nil {
						t.Fatal(err)
					}
					wantRecords := 1
					if scenario == "disabled" || scenario == "judge_off" || scenario == "deferred_disabled" {
						wantRecords = 0
					}
					if scenario == "deferred_both" {
						wantRecords = 2
					}
					if len(rows) != wantRecords {
						t.Fatalf("approval records=%d want=%d", len(rows), wantRecords)
					}
					if wantRecords == 0 {
						return
					}
					row := rows[0]
					if row.TaskID == nil || *row.TaskID != taskID || row.AgentName != agentName || row.Session != session || row.ConversationID != nil {
						t.Fatalf("wrong ownership: %+v", row)
					}
					full, err := d.GetInterceptDetail(row.ID)
					if err != nil || full == nil || full.Audit == nil || full.Audit.ToolUseID != callID || full.Audit.Session != session || full.Audit.Correlation != "exact" {
						t.Fatalf("audit=%+v err=%v", full, err)
					}
					if row.ToolName != reviewedTool || deferred && full.Audit.InvocationTool != invokedTool {
						t.Fatalf("reviewed tool=%s invocation=%s", row.ToolName, full.Audit.InvocationTool)
					}
					if scenario == "deferred_both" {
						outer, err := d.GetInterceptDetail(rows[1].ID)
						if err != nil || outer == nil || outer.Audit == nil || outer.ToolName != actool.ExecuteExtraToolName || outer.Status != "allowed" || outer.Audit.ExecutionStatus != "failed" {
							t.Fatalf("outer approval lost its execution result: %+v err=%v", outer, err)
						}
					}
					wantStatus, wantExecutionStatus := "denied", "not_executed"
					if wantExecution {
						wantStatus, wantExecutionStatus = "allowed", "succeeded"
					}
					if timeout {
						wantStatus = "timeout"
					}
					if scenario == "ask_allow_cancel" || scenario == "model_fail_allow_cancel" {
						wantStatus = "allowed" // keep the human verdict while recording non-execution
					}
					if row.Status != wantStatus || full.Audit.ExecutionStatus != wantExecutionStatus {
						t.Fatalf("status=%s execution=%s want=%s/%s", row.Status, full.Audit.ExecutionStatus, wantStatus, wantExecutionStatus)
					}
					if strings.HasPrefix(scenario, "model_") {
						if judgeCalls != 1 {
							t.Fatalf("judge calls=%d", judgeCalls)
						}
						if full.Audit.ModelFallback != strings.HasPrefix(scenario, "model_fail_") {
							t.Fatalf("model failure flag=%t scenario=%s", full.Audit.ModelFallback, scenario)
						}
					}
					execution, err := d.GetInterceptExecution(row.ID)
					if err != nil || execution.Session != session {
						t.Fatalf("navigation=%+v err=%v", execution, err)
					}
					mu.Lock()
					defer mu.Unlock()
					requests := 0
					for _, a := range activities {
						if a.Worker != agentName || a.NodeID != nil {
							t.Fatalf("wrong activity lane: %+v", a)
						}
						if role == "mainagent" && (a.MainSeg == nil || *a.MainSeg != segment) {
							t.Fatalf("wrong main segment: %+v", a)
						}
						if a.Kind == "intercept_request" {
							requests++
						}
					}
					wantRequests := 0
					if action == "ask" || scenario == "model_ask" || scenario == "model_fail_ask" {
						wantRequests = 1
					}
					if requests != wantRequests {
						t.Fatalf("inline requests=%d want=%d", requests, wantRequests)
					}
				})
			}
		}
	}
}
