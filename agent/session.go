package agent

import (
	"context"
	"encoding/json"

	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// norma's ExecuteExtraTool validates/unlocks a target and then calls it directly,
// bypassing the harness's inner permission and hook path. Wrap the target tools
// so the real name/input retains its policy even when only its schema is deferred.
// Direct SDK calls already pass the harness checks and must not be reviewed twice.
func newGuardedSession(opts agentcore.Options) *agentcore.Session {
	if len(opts.DeferredTools) > 0 {
		if opts.PermissionMode == "" {
			opts.PermissionMode = permission.ModeDefault
		}
		if opts.Tools == nil {
			opts.Tools = actool.DefaultTools()
		}
		deferred := make(map[string]bool, len(opts.DeferredTools))
		for _, name := range opts.DeferredTools {
			deferred[name] = true
		}
		tools := make([]actool.CoreTool, 0, len(opts.Tools))
		for _, tool := range opts.Tools {
			tools = append(tools, delegatedApprovalTool{CoreTool: tool, opts: &opts, deferred: deferred[tool.Name()]})
		}
		opts.Tools = tools
	}
	return agentcore.NewSession(opts)
}

type delegatedApprovalTool struct {
	actool.CoreTool
	opts     *agentcore.Options
	deferred bool
}

func (t delegatedApprovalTool) AcceptsRawInput() bool {
	raw, ok := t.CoreTool.(interface{ AcceptsRawInput() bool })
	return ok && raw.AcceptsRawInput()
}

func (t delegatedApprovalTool) CheckPermissions(ctx context.Context, input json.RawMessage, pc permission.Context) permission.Decision {
	if t.deferred && t.opts.UnlockSet != nil && !t.opts.UnlockSet.Has(t.Name()) {
		return permission.Denied("tool " + t.Name() + " is not unlocked in this session — load the skill that provides it first")
	}
	return t.CoreTool.CheckPermissions(ctx, input, pc)
}

func (t delegatedApprovalTool) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	if tc != nil && intercept.InvocationTool(ctx, tc.ToolUseID) == t.Name() {
		return t.CoreTool.Call(ctx, input, tc)
	}
	pc := permission.Context{Mode: t.opts.PermissionMode, WorkingDir: t.opts.WorkingDir, Allowed: t.opts.AllowedTools, Disallowed: t.opts.DisallowedTools}
	decision := permission.Evaluate(ctx, permission.Request{ToolName: t.Name(), Input: input,
		IsReadOnly: t.IsReadOnly(input), ToolDecision: t.CheckPermissions(ctx, input, pc), Ctx: pc, Ask: t.opts.CanUseTool})
	if decision.Behavior != permission.Allow {
		return actool.Errorf("Tool call was not permitted: " + decision.Message), nil
	}
	if len(decision.UpdatedInput) > 0 {
		input = decision.UpdatedInput
	}
	if tc != nil {
		ctx = intercept.WithDelegatedCall(ctx, tc.ToolUseID, t.Name(), input)
	}
	if t.opts.Hooks != nil {
		blocked, message, updated := t.opts.Hooks.PreToolUse(ctx, t.Name(), input)
		if blocked {
			return actool.Errorf("Blocked by hook: " + message), nil
		}
		if len(updated) > 0 {
			input = updated
		}
	}
	if ctx.Err() != nil {
		return actool.Errorf("工作已取消，平台安全管控阻止执行"), nil
	}
	result, err := t.CoreTool.Call(ctx, input, tc)
	if err != nil {
		result = actool.Errorf("Error: " + err.Error())
	}
	if t.opts.Hooks != nil {
		raw, _ := json.Marshal(result.Flatten())
		t.opts.Hooks.PostToolUse(ctx, t.Name(), input, raw, result.IsError)
	}
	return result, nil
}
