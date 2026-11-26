package guard

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// 强制全局代理下，不读 *_PROXY 的裸连工具必须被拦下——否则它们会从本机真实
// IP 直连出网（生产环境因此吃过机房 Abuse 投诉，见 guard.enforceProxyOnly）。
func TestForceProxyBlocksProxyBlindTools(t *testing.T) {
	g := New()
	g.SetForceProxy(func() bool { return true })

	block := func(cmd string) (bool, string) {
		input, _ := json.Marshal(map[string]string{"command": cmd})
		b, msg, _ := g.Hooks().PreToolUse(context.Background(), "Bash", input)
		return b, msg
	}

	blocked := []string{
		`nmap -sV -p- 10.0.0.0/24`,
		`nmap -sS 1.2.3.4`,
		`masscan -p1-65535 1.2.3.4 --rate 1000`,
		`hydra -l admin -P rockyou.txt ssh://1.2.3.4`,
		`nc 1.2.3.4 4444`,
		`dig @8.8.8.8 example.com`,
		`whois example.com`,
		`ping -c 3 1.2.3.4`,
		`echo x; nmap 1.2.3.4`,
	}
	for _, cmd := range blocked {
		if ok, msg := block(cmd); !ok {
			t.Errorf("force-proxy 应拦截裸连工具，未拦截: %q", cmd)
		} else if !strings.Contains(msg, "强制全局代理") {
			t.Errorf("拦截原因应说明是强制代理导致，得到: %q", msg)
		}
	}

	// 读 *_PROXY 的 HTTP 客户端照常放行。
	allowed := []string{
		`curl -sS https://example.com/`,
		`wget -qO- https://example.com/`,
		`python3 -c "import requests;requests.get('https://example.com')"`,
		`subfinder -d example.com`,
		`httpx -u https://example.com`,
		`nuclei -u https://example.com`,
		`ffuf -u https://example.com/FUZZ -w common.txt`,
		`sqlmap -u "http://example.com/?id=1" --batch`,
		`ls -la /tmp`,
	}
	for _, cmd := range allowed {
		if ok, _ := block(cmd); ok {
			t.Errorf("走代理的工具不应被拦截: %q", cmd)
		}
	}

	// 命令自己显式指定了代理时放行（操作者已经决定怎么出网）。
	explicit := []string{
		`nmap --proxies socks4://127.0.0.1:8788 -sV 1.2.3.4`,
		`curl -x socks5h://127.0.0.1:8788 https://example.com`,
		`proxychains4 nmap -sV 1.2.3.4`,
		`curl --proxy http://127.0.0.1:8788 https://example.com`,
		`HTTPS_PROXY=http://127.0.0.1:8788 curl https://example.com`,
	}
	for _, cmd := range explicit {
		if ok, _ := block(cmd); ok {
			t.Errorf("显式带代理参数的命令不应被拦截: %q", cmd)
		}
	}
}

// 关掉强制代理（或没接解析器）时行为完全不变，避免影响既有部署。
func TestForceProxyGateDisabledByDefault(t *testing.T) {
	for _, g := range []*Guard{New(), func() *Guard {
		g := New()
		g.SetForceProxy(func() bool { return false })
		return g
	}()} {
		input, _ := json.Marshal(map[string]string{"command": "nmap -sV 1.2.3.4"})
		if blocked, _, _ := g.Hooks().PreToolUse(context.Background(), "Bash", input); blocked {
			t.Error("未开启强制代理时不应拦截 nmap")
		}
	}
}

// 交互式 shell（shell_open / shell_send）同样要过这道闸，agent 默认走的是它。
func TestForceProxyGateCoversInteractiveShell(t *testing.T) {
	g := New()
	g.SetForceProxy(func() bool { return true })

	open, _ := json.Marshal(map[string]string{"command": "nmap -sV 1.2.3.4"})
	if blocked, _, _ := g.Hooks().PreToolUse(context.Background(), "shell_open", open); !blocked {
		t.Error("shell_open 应以同样规则拦截裸连工具")
	}
	send, _ := json.Marshal(map[string]string{"text": "hydra -l a -P p.txt 1.2.3.4 ssh"})
	if blocked, _, _ := g.Hooks().PreToolUse(context.Background(), "shell_send", send); !blocked {
		t.Error("shell_send 应以同样规则拦截裸连工具")
	}
	// 非 shell 类工具（Read/Write…）不受影响
	if blocked, _, _ := g.Hooks().PreToolUse(context.Background(), "Read", []byte(`{}`)); blocked {
		t.Error("Read 不应被出口闸门拦截")
	}
}
