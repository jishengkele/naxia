package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

func TestTaskProcessScopePreservesEnvironmentAndDeferredApproval(t *testing.T) {
	old := ProcessScope
	t.Cleanup(func() { ProcessScope = old })
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "registered", true: "registration_failed"}[fail], func(t *testing.T) {
			registered := 0
			executed := 0
			ProcessScope = func(ri RunInfo, session string) ([]string, error) {
				registered++
				if ri.TaskID != 6 || ri.IntentID != 552 || session != "fixture" {
					t.Fatalf("wrong attribution: %+v %s", ri, session)
				}
				if fail {
					return nil, errors.New("disk full")
				}
				return []string{"NAXIA_PROCESS_SCOPE=fixture-token"}, nil
			}
			probe := actool.Build(actool.Spec{Name: "ProcessProbe", Schema: map[string]any{"type": "object"}, Run: func(_ context.Context, _ json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
				executed++
				if !slices.Contains(tc.Env, "HTTPS_PROXY=fixture") || !slices.Contains(tc.Env, "NAXIA_PROCESS_SCOPE=fixture-token") {
					t.Fatalf("environment lost: %v", tc.Env)
				}
				return actool.Text("ok"), nil
			}})
			turn := 0
			p := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
				turn++
				events := []llm.StreamEvent{{Type: llm.SETextDelta, Text: "done"}, {Type: llm.SEMessageDelta, StopReason: "end_turn"}, {Type: llm.SEMessageStop}}
				if turn == 1 {
					events = []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: "scope-call", ToolName: actool.ExecuteExtraToolName}, {Type: llm.SEToolInputJSON, Text: `{"tool_name":"ProcessProbe","params":{}}`}, {Type: llm.SEMessageDelta, StopReason: "tool_use"}, {Type: llm.SEMessageStop}}
				}
				for _, event := range events {
					if !yield(event, nil) {
						return
					}
				}
			}}
			ctx := WithRunInfo(t.Context(), RunInfo{TaskID: 6, IntentID: 552})
			_, _, err := captureRun(ctx, agentcore.Options{Provider: p, SessionID: "fixture", Tools: []actool.CoreTool{probe}, BashEnv: []string{"HTTPS_PROXY=fixture"}, DeferredTools: []string{"ProcessProbe"}, UnlockSet: actool.NewUnlockSet("ProcessProbe"), PermissionMode: permission.ModeBypass, DisableBackgroundTasks: true, MaxTurns: 2}, "fixture", nil)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if fail {
				want = 0
			}
			if registered != 1 || executed != want {
				t.Fatalf("registered=%d executed=%d", registered, executed)
			}
		})
	}
}
