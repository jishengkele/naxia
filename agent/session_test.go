package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/hook"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

func TestDeferredInvocationPreservesPolicyAndUnlock(t *testing.T) {
	for _, nonStreaming := range []bool{false, true} {
		for _, scenario := range []string{"allow", "deny_inner", "deny_outer", "locked", "invalid", "hard_deny", "direct", "direct_locked"} {
			t.Run(fmt.Sprintf("sync_%t/%s", nonStreaming, scenario), func(t *testing.T) {
				const name = "DeferredProbe"
				var executed atomic.Int64
				probe := actool.Build(actool.Spec{Name: name,
					Schema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}, "required": []any{"value"}},
					Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
						if scenario == "hard_deny" {
							return permission.Denied("fixture permission denied")
						}
						return permission.Allowed()
					},
					Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
						executed.Add(1)
						return actool.Text("executed"), nil
					}})
				var mu sync.Mutex
				var before, after []string
				hooks := hook.NewRegistry().On(hook.PreToolUse, func(_ context.Context, ev hook.Event) hook.Result {
					mu.Lock()
					defer mu.Unlock()
					before = append(before, ev.ToolName)
					if scenario == "deny_inner" && ev.ToolName == name || scenario == "deny_outer" && ev.ToolName == actool.ExecuteExtraToolName {
						return hook.Result{Decision: "block", Message: "fixture denial"}
					}
					return hook.Result{}
				}).On(hook.PostToolUse, func(_ context.Context, ev hook.Event) hook.Result {
					mu.Lock()
					defer mu.Unlock()
					after = append(after, ev.ToolName)
					return hook.Result{}
				})
				unlock := actool.NewUnlockSet(name)
				if scenario == "locked" || scenario == "direct_locked" {
					unlock = actool.NewUnlockSet()
				}
				params := json.RawMessage(`{"value":"fixture"}`)
				if scenario == "invalid" {
					params = json.RawMessage(`{}`)
				}
				input, _ := json.Marshal(map[string]any{"tool_name": name, "params": params})
				invoked := actool.ExecuteExtraToolName
				if scenario == "direct" || scenario == "direct_locked" {
					invoked, input = name, params
				}
				turn := 0
				provider := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
					turn++
					events := []llm.StreamEvent{{Type: llm.SETextDelta, Text: "done"}, {Type: llm.SEMessageDelta, StopReason: "end_turn"}, {Type: llm.SEMessageStop}}
					if turn == 1 {
						events = []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: "deferred-call", ToolName: invoked}, {Type: llm.SEToolInputJSON, Text: string(input)}, {Type: llm.SEMessageDelta, StopReason: "tool_use"}, {Type: llm.SEMessageStop}}
					}
					for _, event := range events {
						if !yield(event, nil) {
							return
						}
					}
				}}
				_, _, err := captureRun(t.Context(), agentcore.Options{Provider: provider, Tools: []actool.CoreTool{probe},
					DeferredTools: []string{name}, UnlockSet: unlock, Hooks: hooks, PermissionMode: permission.ModeBypass,
					DisableBackgroundTasks: true, NonStreaming: nonStreaming, MaxTurns: 2}, "run fixture", nil)
				if err != nil {
					t.Fatal(err)
				}
				wantExecution := scenario == "allow" || scenario == "direct"
				if (executed.Load() == 1) != wantExecution {
					t.Fatalf("executions=%d scenario=%s", executed.Load(), scenario)
				}
				mu.Lock()
				defer mu.Unlock()
				innerCalls := 0
				for _, called := range before {
					if called == name {
						innerCalls++
					}
				}
				wantInner := 0
				if scenario == "allow" || scenario == "deny_inner" || scenario == "direct" {
					wantInner = 1
				}
				if innerCalls != wantInner || slices.Contains(after, name) != wantExecution {
					t.Fatalf("pre=%v post=%v want inner=%d execution=%t", before, after, wantInner, wantExecution)
				}
			})
		}
	}
}
