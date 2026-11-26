#!/usr/bin/env bash
#
# 部署后收尾：探活 → 补齐 2C2G 口径的运行设置 → 验收。
# 只在「镜像已经起来、还没配任何东西」的新机器上跑；已经跑过的话重复执行也安全（幂等）。
#
# 用法：
#   NAXIA_PASS='你的后台密码' bash deploy/post-deploy.sh
#   NAXIA_PASS='...' BASE=https://your.domain bash deploy/post-deploy.sh
#
# 需要：curl、python3。会在本机直连 API（默认 http://127.0.0.1:8787），不经过反代。
set -uo pipefail

BASE="${BASE:-http://127.0.0.1:8787}"
USER_NAME="${NAXIA_USER:-NAXIA}"
PASS="${NAXIA_PASS:-}"

if [ -z "$PASS" ]; then
  echo "用法：NAXIA_PASS='后台密码' bash $0" >&2
  exit 2
fi
command -v curl >/dev/null || { echo "缺少 curl" >&2; exit 2; }
command -v python3 >/dev/null || { echo "缺少 python3" >&2; exit 2; }

json() { python3 -c "import sys,json;$1" ; }

echo "===== 1. 等待服务就绪 ====="
code=""
for i in $(seq 1 60); do
  code=$(curl -s -o /tmp/pd_health.json -w '%{http_code}' "$BASE/api/health" 2>/dev/null || true)
  [ "$code" = "200" ] && break
  sleep 2
done
if [ "$code" != "200" ]; then
  echo "健康检查未通过（http=$code），先看 docker compose logs artex" >&2
  exit 1
fi
cat /tmp/pd_health.json; echo

echo "===== 2. 登录 ====="
TOKEN=$(curl -s -X POST "$BASE/api/auth/login" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$USER_NAME\",\"password\":\"$PASS\"}" \
  | json 'print(json.load(sys.stdin).get("token",""))')
if [ -z "$TOKEN" ]; then
  echo "登录失败：密码不对，或首次部署还没初始化（先打开 /setup 设置密码）" >&2
  exit 1
fi
AUTH="Authorization: Bearer $TOKEN"
echo "已登录（token ${#TOKEN} 字符）"

echo "===== 3. 写入 2C2G 口径的运行设置 ====="
curl -s -X PUT "$BASE/api/settings" -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "workers": 2,
  "task_concurrency_enabled": true,
  "task_concurrency_limit": 2,
  "llm_pool_enabled": true
}' -o /dev/null -w 'settings: %{http_code}\n'

# 任务情报（凭据/线索的结构化归档）——默认关闭，这里按需开启。
curl -s -X PUT "$BASE/api/settings" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"task_intelligence_enabled": true}' -o /dev/null -w 'task_intelligence: %{http_code}\n'

echo "===== 4. 是否已有可用 LLM 配置 ====="
HAS_PROFILE=$(curl -s "$BASE/api/llm/profiles" -H "$AUTH" \
  | json 'd=json.load(sys.stdin);print(len(d.get("profiles") or []))')
echo "profiles = $HAS_PROFILE"

if [ "$HAS_PROFILE" -gt 0 ]; then
  echo "===== 5. 启用 fallback judge（规则盲区交给模型审） ====="
  curl -s "$BASE/api/intercept/judge" -H "$AUTH" -o /tmp/pd_judge.json
  json 'import json;d=json.load(open("/tmp/pd_judge.json"));d["enabled"]=True;json.dump(d,open("/tmp/pd_judge.json","w"),ensure_ascii=False)' 2>/dev/null \
  && curl -s -X PUT "$BASE/api/intercept/judge" -H "$AUTH" -H 'Content-Type: application/json' \
       --data-binary @/tmp/pd_judge.json -o /dev/null -w 'judge: %{http_code}\n'

  echo "===== 6. 启用 browser MCP（Playwright，能过 JS/SPA） ====="
  curl -s "$BASE/api/mcp" -H "$AUTH" -o /tmp/pd_mcp.json
  BODY=$(python3 - <<'PY'
import json
d = json.load(open('/tmp/pd_mcp.json'))
srv = next((s for s in d.get('servers', []) if s.get('name') == 'browser'), None)
if not srv:
    print(''); raise SystemExit
srv['enabled'] = True
print(json.dumps(srv, ensure_ascii=False))
PY
)
  if [ -n "$BODY" ]; then
    printf '%s' "$BODY" > /tmp/pd_mcp_one.json
    curl -s -X POST "$BASE/api/mcp" -H "$AUTH" -H 'Content-Type: application/json' \
      --data-binary @/tmp/pd_mcp_one.json -o /dev/null -w 'mcp browser: %{http_code}\n'
  else
    echo "mcp browser: 未找到内置 browser MCP，跳过"
  fi
else
  echo "===== 5/6. 还没有 LLM 配置，跳过 judge 与 browser MCP ====="
  echo "  先到「系统 → LLM」加一个 profile，再重跑本脚本。"
fi

echo "===== 7. 验收 ====="
curl -s "$BASE/api/settings" -H "$AUTH" | json '
import sys,json
d=json.load(sys.stdin)
for k in ("workers","task_concurrency_enabled","task_concurrency_limit","llm_pool_enabled","task_intelligence_enabled","traffic_capture","egress_mode"):
    print(f"  {k} = {d.get(k)}")
'
curl -s "$BASE/api/intercept/judge" -H "$AUTH" | json 'import sys,json;print("  judge enabled =", json.load(sys.stdin).get("enabled"))'
curl -s "$BASE/api/mcp" -H "$AUTH" | json 'import sys,json;print("  mcp:", [(s["name"], s["enabled"]) for s in json.load(sys.stdin)["servers"]])'
curl -s "$BASE/api/task-templates" -H "$AUTH" | json 'import sys,json;d=json.load(sys.stdin);print("  templates:", [(t["id"], t["name"]) for t in d["templates"]])'
curl -s "$BASE/api/agents" -H "$AUTH" | json 'import sys,json;d=json.load(sys.stdin);print("  pentest run_seconds =", [a["run_seconds"] for a in d["agents"] if a["key"]=="pentest"])'

echo "===== 8. 容器里的工具 ====="
docker exec naxia-artex-1 sh -c 'for t in subfinder httpx nuclei katana ffuf gau sqlmap; do printf "%-10s " $t; command -v $t >/dev/null && echo ok || echo MISSING; done' 2>/dev/null \
  || echo "（不是在 docker 部署目录里执行，跳过）"

echo
echo "完成。剩余需要手动做的：LLM 配置（若还没配）、全局出口代理（可选）、域名证书。"
