// Package guard implements the safety boundary layer (docs §11): an audit log,
// user-configured intercept-rule evaluation, and Observer/G5 failure attribution.
// Every tool call passes through the PreToolUse hook before executing.
// (The RoE authorization-scope mechanism was removed; a replacement may be added
// later.) Destructive/exfil gating is no longer hard-coded here — it lives in the
// DB intercept rules (seeded as ordinary [内置] rules, so users can disable or
// delete them), evaluated via applyIntercept.
package guard

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jishengkele/naxia/intercept"
	"github.com/Autumn-27/norma/hook"
)

// AuditEntry records one gated tool call.
type AuditEntry struct {
	TS      int64  `json:"ts"`
	Tool    string `json:"tool"`
	Action  string `json:"action"` // allow|block
	Reason  string `json:"reason,omitempty"`
	Command string `json:"command,omitempty"`
}

// Guard enforces the side-effect policy via agent-core hooks.
type Guard struct {
	mu          sync.Mutex
	audit       []AuditEntry
	attrib      map[string]int // failure attribution counts (Observer / G5)
	reg         *hook.Registry
	interceptor *intercept.Interceptor // optional; nil disables user-configured rules
	// forceProxy, when set and returning true, makes this guard refuse tool calls
	// that would leave the host WITHOUT going through the configured egress proxy.
	//
	// 背景：强制全局代理本质上只是给子进程注入 HTTP_PROXY/HTTPS_PROXY/ALL_PROXY。
	// curl / wget / python-requests / Go 写的工具（subfinder、httpx、nuclei、
	// katana、ffuf、gau）会读这些变量，但 nmap / masscan / hydra / nc / dig /
	// whois 这类直接开 TCP/UDP socket 的工具完全无视它们，照样从本机 IP 出网——
	// 在只允许代理出站的场景下这就是真实源 IP 泄漏（生产环境曾因此被机房
	// Abuse 投诉）。所以"强制"必须在调用层兑现：这类命令一律拦下，除非命令
	// 自己显式带了代理参数。
	forceProxy func() bool
}

// New creates a Guard without user-configured intercept rules (used for pentest
// tasks where the Interceptor is not yet available).
func New() *Guard { return newGuard(nil) }

// NewWithInterceptor creates a Guard with user-configured intercept rules.
func NewWithInterceptor(ic *intercept.Interceptor) *Guard { return newGuard(ic) }

// SetForceProxy wires the resolver deciding whether "force all egress through the
// global proxy" is currently in effect. nil (or a false return) disables the
// proxy-blind tool gate, leaving previous behavior untouched.
func (g *Guard) SetForceProxy(fn func() bool) { g.forceProxy = fn }

func newGuard(ic *intercept.Interceptor) *Guard {
	g := &Guard{attrib: map[string]int{}, interceptor: ic}
	g.reg = hook.NewRegistry().
		On(hook.PreToolUse, g.preToolUse).
		On(hook.PostToolUse, g.postToolUse)
	return g
}

// Hooks returns the hook registry to attach to an agent session.
func (g *Guard) Hooks() *hook.Registry { return g.reg }

func (g *Guard) preToolUse(ctx context.Context, ev hook.Event) hook.Result {
	// Extract the shell-command surface for the audit log: Bash + the interactive-shell
	// tools (shell_open's command, shell_send's text). Destructive/exfil gating is no
	// longer hard-coded here — it now lives in the DB intercept rules, evaluated by
	// applyIntercept below. Other tools record an empty command.
	var cmd string
	switch ev.ToolName {
	case "Bash", "shell_open":
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(ev.Input, &in)
		cmd = in.Command
	case "shell_send":
		var in struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(ev.Input, &in)
		cmd = in.Text
	}
	g.record(ev.ToolName, "allow", "", cmd)
	if res, blocked := g.enforceProxyOnly(ev.ToolName, cmd); blocked {
		return res
	}
	return g.applyIntercept(ctx, ev)
}

// reProxyBlindTool matches commands whose tool opens TCP/UDP sockets itself and
// therefore ignores HTTP_PROXY / HTTPS_PROXY / ALL_PROXY. These are exactly the
// commands that leak the host's real egress IP under a force-proxy policy.
//
// Deliberately conservative: only unambiguous network tools. A word like "host"
// or "ssh" is left out either because it is too common in prose/paths or because
// blocking it would break routine operations that the operator can proxy at the
// SSH level.
var reProxyBlindTool = regexp.MustCompile(`(?i)(?:^|[\s;&|(){\[` + "`" + `])(nmap|masscan|zmap|hydra|medusa|ncrack|ncat|netcat|nc|socat|dig|drill|nslookup|whois|telnet|hping3|traceroute|mtr|smbclient|redis-cli|ping)\b`)

// reExplicitProxy matches the command-line forms that DO route through a proxy:
// nmap --proxies, curl -x/--proxy, common --socks5 flags, proxychains, and an
// explicit *_PROXY= prefix. A command carrying one of these passes the gate —
// the operator has already decided how it should egress.
var reExplicitProxy = regexp.MustCompile(`(?i)(--proxies|--proxy[=\s]|\s-x[=\s]|--socks[45]h?[=\s]|proxychains|(?:https?|all)_proxy=)`)

// enforceProxyOnly blocks proxy-blind network tools while "force all egress
// through the global proxy" is on. Without this the policy is advisory only:
// the injected *_PROXY variables are honored by HTTP clients but not by raw
// socket tools, so scans would still leave from the host's own IP.
func (g *Guard) enforceProxyOnly(tool, cmd string) (hook.Result, bool) {
	if g.forceProxy == nil || !g.forceProxy() || strings.TrimSpace(cmd) == "" {
		return hook.Result{}, false
	}
	if reExplicitProxy.MatchString(cmd) {
		return hook.Result{}, false
	}
	m := reProxyBlindTool.FindStringSubmatch(cmd)
	if m == nil {
		return hook.Result{}, false
	}
	name := strings.ToLower(m[1])
	reason := systemBlockMessage("当前是「强制全局代理」模式，而 " + name +
		" 直接开 socket 出网、不读取 HTTP_PROXY/ALL_PROXY，会从本机真实 IP 直连，属于出口泄漏。" +
		"请改用会走代理的等效做法（curl / wget / httpx / nuclei / ffuf / sqlmap / python-requests），" +
		"或给命令显式指定代理（如 nmap --proxies socks4://127.0.0.1:8788、proxychains " + name + " …）后再试")
	g.record(tool, "block", "强制代理下拦截直连工具: "+name, cmd)
	return hook.Result{Decision: "block", Message: reason}, true
}

// applyIntercept evaluates user-configured intercept rules against the tool call.
// Both rules and the fallback judge receive the complete tool input.
func (g *Guard) applyIntercept(ctx context.Context, ev hook.Event) (result hook.Result) {
	reviewed := false
	defer func() {
		// Stop/pause is independent of allow/deny/ask policy. In particular, a
		// cancelled model request must not run a tool via FailAction=allow.
		if ctx.Err() != nil {
			if reviewed {
				g.interceptor.CancelCall(ctx)
			}
			result = g.block(ev.ToolName, systemBlockMessage("工作已取消，平台安全管控阻止执行"), "")
		}
	}()
	if g.interceptor == nil {
		return hook.Result{}
	}
	if !g.interceptor.IsToolEnabled(ev.ToolName) {
		return hook.Result{}
	}
	ctx = intercept.WithCall(ctx, ev.ToolName, ev.Input)
	reviewed = true
	dec, matched := g.interceptor.Match(ev.ToolName, ev.Input)
	if !matched {
		// No rule matched. Ask the LLM fallback judge (if enabled); when it is off
		// or unwired, keep current behavior and allow.
		d, judged := g.interceptor.Judge(ctx, ev.ToolName, ev.Input)
		if !judged {
			return hook.Result{}
		}
		dec = d
	}
	switch dec.Action {
	case "deny":
		// 观测:deny 命中不阻塞审批,直接记一条 denied（历史/任务拦截页可见）。
		g.interceptor.Log(ctx, intercept.ConvIDFromContext(ctx), dec, ev.ToolName, ev.Input, "denied")
		return g.block(ev.ToolName, systemBlockMessage(dec.Message), "")
	case "allow":
		// Record explicit rule and model approvals so review details remain auditable.
		g.interceptor.Log(ctx, intercept.ConvIDFromContext(ctx), dec, ev.ToolName, ev.Input, "allowed")
		return hook.Result{}
	case "ask":
		// If the worker context is already cancelled (task stopped / killed), block
		// immediately without creating a pending record — avoids orphaned DB entries
		// and makes execOne complete fast, reducing the race against drainSynthetic.
		if ctx.Err() != nil {
			return g.block(ev.ToolName, systemBlockMessage("工作已取消，平台安全管控阻止执行"), "")
		}
		convID := intercept.ConvIDFromContext(ctx)
		if !g.interceptor.HandleAsk(ctx, convID, dec, ev.ToolName, ev.Input) {
			return g.block(ev.ToolName, systemBlockMessage("人工审批未通过（用户拒绝或审批超时）"), "")
		}
		return hook.Result{}
	}
	return hook.Result{}
}

// systemBlockMessage frames an intercept block as an NAXIA platform-governance
// decision so the agent does not mistake it for a target-side defense.
//
// The bare reasons ("禁止执行此工具" / "用户拒绝") read exactly like a WAF/403 on
// the target, so a pentest agent's instinct is to bypass them — rewrite the
// command, swap the payload, re-encode, retry. That is both futile (the platform
// blocks the class of action, not one string) and wrong (it's a policy decision,
// not an obstacle to defeat). This prefix states plainly that the block comes
// from the platform, is not the target's protection, and that the operation is
// forbidden — so the agent pivots to another approach instead of evading it.
// Audit/history rows keep the raw reason (see Interceptor.Log); only the
// model-facing tool_result carries this framing.
func systemBlockMessage(reason string) string {
	return "【NAXIA 平台管控·非目标防御】此调用被平台拦截。" +
		"原因：" + reason + "。此操作被禁止。"
}

var reBlocked = regexp.MustCompile(`(?i)\b(403|forbidden|waf|blocked|rate.?limit|429|captcha|denied)\b`)

// postToolUse is the Observer failure-attribution hook (G5): it classifies tool
// results into blocked / error / ok so the planner can change strategy instead
// of giving up at a WAF.
func (g *Guard) postToolUse(_ context.Context, ev hook.Event) hook.Result {
	if ev.ToolName != "Bash" {
		return hook.Result{}
	}
	class := "ok"
	switch {
	case reBlocked.Match(ev.Result):
		class = "blocked"
	case ev.IsError:
		class = "error"
	}
	g.mu.Lock()
	g.attrib[class]++
	g.mu.Unlock()
	return hook.Result{}
}

// Attributions returns failure-attribution counts (Observer / G5).
func (g *Guard) Attributions() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]int, len(g.attrib))
	for k, v := range g.attrib {
		out[k] = v
	}
	return out
}

func (g *Guard) block(tool, reason, cmd string) hook.Result {
	g.record(tool, "block", reason, cmd)
	return hook.Result{Decision: "block", Message: reason}
}

func (g *Guard) record(tool, action, reason, cmd string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.audit = append(g.audit, AuditEntry{TS: time.Now().Unix(), Tool: tool, Action: action, Reason: reason, Command: cmd})
	if len(g.audit) > 2000 {
		g.audit = g.audit[len(g.audit)-2000:]
	}
}

// Audit returns a snapshot of recent gated calls (most recent last).
func (g *Guard) Audit() []AuditEntry {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]AuditEntry, len(g.audit))
	copy(out, g.audit)
	return out
}
