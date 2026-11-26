# syntax=docker/dockerfile:1
#
# 运行镜像（不在镜像里编译）：只装常用工具，放入**预编译好的 Linux 单二进制**。
# 二进制由 CI 的 binaries job 交叉编译（纯 Go、无 QEMU），按目标架构放在
# 构建上下文的 dist/<TARGETARCH>/artex。这样多架构构建时 arm64 只需模拟 apt 层，
# 不再模拟 Next/Go 编译，速度快得多。
#
# 本地手动构建镜像时，先自行准备二进制：
#   cd web && npm run build:static && cd ..
#   mkdir -p server/webui && cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   docker build -t artex:local .
FROM python:3.12-slim-bookworm
ARG TARGETARCH

# 基础工具 + 常用渗透件（whatweb / hydra 来自 apt，两个架构都有）。
# Node 从 NodeSource 装 22.x，与前端构建使用的主版本一致。
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
      whatweb hydra \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# 预装 Playwright MCP 与 CLI（全局），运行时不再 npx 联网下载。
# @playwright/mcp：browser MCP 直接 `npx @playwright/mcp`（已全局装好，无需 -y/@latest）。
# @playwright/cli：提供 playwright-cli，装完顺带 --help 验证可执行。
# 再装 playwright（提供浏览器管理），装完用 --with-deps 预置 chromium 及其系统依赖，
# 这样容器内 MCP/CLI 首次启动即可用，不再联网下载浏览器。
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*

# Python 侧工具：sqlmap（纯 Python，无架构问题）+ 常用库（写探测脚本用）。
#
# 必须放在 ProjectDiscovery 工具**之前**：pip 装的 httpx 库自带同名 console script
# （/usr/local/bin/httpx），后装会把 PD 的 httpx 二进制覆盖掉，运行时表现为
# "The httpx command line client could not run because the required dependencies were not installed"。
RUN pip install --no-cache-dir sqlmap requests httpx beautifulsoup4 lxml pyyaml dnspython \
    && sqlmap --version | head -1

# 渗透侦察工具链：任务模板里点名要用的 subfinder / httpx / nuclei / katana / ffuf / gau。
#
# 版本**写死**并直接用 releases/download/<tag>/<file> 直链下载——刻意不用
# api.github.com 的 "latest release" 接口：匿名调用额度是每 IP 60 次/小时，
# 构建机或 CI runner 上经常已经被别的任务打满，会随机 403 把构建打红。
# 升级方式：改下面这几个版本号与文件名（两处要一致），其余不用动。
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) ARCH=amd64 ;; \
      arm64) ARCH=arm64 ;; \
      *) echo "unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    tmp="$(mktemp -d)"; \
    zp() { \
      url="$1"; tool="$2"; \
      curl -fsSL --retry 3 -o "${tmp}/${tool}.zip" "${url}"; \
      unzip -o -q "${tmp}/${tool}.zip" -d "${tmp}/${tool}"; \
      install -m 0755 "${tmp}/${tool}/${tool}" "/usr/local/bin/${tool}"; \
    }; \
    tp() { \
      url="$1"; tool="$2"; \
      curl -fsSL --retry 3 -o "${tmp}/${tool}.tar.gz" "${url}"; \
      tar -xzf "${tmp}/${tool}.tar.gz" -C "${tmp}" "${tool}"; \
      install -m 0755 "${tmp}/${tool}" "/usr/local/bin/${tool}"; \
    }; \
    zp "https://github.com/projectdiscovery/subfinder/releases/download/v2.17.0/subfinder_2.17.0_linux_${ARCH}.zip" subfinder; \
    zp "https://github.com/projectdiscovery/httpx/releases/download/v1.12.0/httpx_1.12.0_linux_${ARCH}.zip" httpx; \
    zp "https://github.com/projectdiscovery/nuclei/releases/download/v3.11.1/nuclei_3.11.1_linux_${ARCH}.zip" nuclei; \
    zp "https://github.com/projectdiscovery/katana/releases/download/v1.8.0/katana_1.8.0_linux_${ARCH}.zip" katana; \
    tp "https://github.com/ffuf/ffuf/releases/download/v2.3.0/ffuf_2.3.0_linux_${ARCH}.tar.gz" ffuf; \
    tp "https://github.com/lc/gau/releases/download/v2.2.4/gau_2.2.4_linux_${ARCH}.tar.gz" gau; \
    rm -rf "${tmp}"; \
    for t in subfinder httpx nuclei katana ffuf gau; do \
      test -x "/usr/local/bin/${t}" || { echo "missing ${t}" >&2; exit 1; }; \
    done; \
    httpx -version 2>&1 | head -3; \
    ls -l /usr/local/bin/subfinder /usr/local/bin/httpx /usr/local/bin/nuclei /usr/local/bin/katana /usr/local/bin/ffuf /usr/local/bin/gau

# nuclei 模板（约 10MB 压缩 / 84MB 解压，1.4 万个模板）。
# 不用 `nuclei -update-templates`：它先查 api.github.com 的 release，而匿名额度
# 每 IP 60 次/小时，构建机上经常 403 —— 表现为只打 banner、一个模板都不下。
# 直接拉 codeload 的 main 分支 tarball，稳定且不需要 API。
RUN set -eux; \
    curl -fsSL --retry 3 -o /tmp/tpl.tar.gz \
      https://codeload.github.com/projectdiscovery/nuclei-templates/tar.gz/refs/heads/main; \
    tar -xzf /tmp/tpl.tar.gz -C /tmp; \
    mkdir -p /root/nuclei-templates; \
    cp -a /tmp/nuclei-templates-main/. /root/nuclei-templates/; \
    rm -rf /tmp/tpl.tar.gz /tmp/nuclei-templates-main; \
    n="$(find /root/nuclei-templates -name '*.yaml' | wc -l)"; \
    echo "templates: ${n} yaml"; \
    test "${n}" -gt 1000

# SecLists 精简子集：只取模板里真正会用到的几份，避免整包 1GB+。
# /usr/share/wordlists 指向该目录，兼容习惯用 rockyou 路径的脚本。
RUN set -eux; \
    base="https://raw.githubusercontent.com/danielmiessler/SecLists/master"; \
    mkdir -p /usr/share/seclists/Discovery/Web-Content \
             /usr/share/seclists/Discovery/DNS \
             /usr/share/seclists/Discovery/Web-Content/api \
             /usr/share/seclists/Passwords/Common-Credentials; \
    get() { curl -fsSL --retry 3 -o "$2" "$base/$1"; }; \
    get "Discovery/Web-Content/common.txt"                   /usr/share/seclists/Discovery/Web-Content/common.txt; \
    get "Discovery/Web-Content/raft-small-directories.txt"   /usr/share/seclists/Discovery/Web-Content/raft-small-directories.txt; \
    get "Discovery/Web-Content/raft-medium-directories.txt"  /usr/share/seclists/Discovery/Web-Content/raft-medium-directories.txt; \
    get "Discovery/Web-Content/raft-small-files.txt"         /usr/share/seclists/Discovery/Web-Content/raft-small-files.txt; \
    get "Discovery/DNS/subdomains-top1million-5000.txt"      /usr/share/seclists/Discovery/DNS/subdomains-top1million-5000.txt; \
    get "Discovery/Web-Content/api/api-endpoints.txt"        /usr/share/seclists/Discovery/Web-Content/api/api-endpoints.txt; \
    get "Passwords/Common-Credentials/10k-most-common.txt"   /usr/share/seclists/Passwords/Common-Credentials/10k-most-common.txt; \
    ln -sfn /usr/share/seclists /usr/share/wordlists; \
    du -sh /usr/share/seclists

WORKDIR /app
# 预编译好的对应架构二进制（dist/amd64/artex 或 dist/arm64/artex）
COPY dist/${TARGETARCH}/artex /app/artex
# 守护启动脚本：进程退出后按退出码决定是否重新拉起，页面一键更新靠它完成换装。
# 它同时负责把 SIGTERM 转发给 artex —— docker stop 只把信号发给 PID 1，
# 不转发的话 artex 收不到、做不了优雅关闭，10 秒后被 SIGKILL 硬杀。
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# data/（SQLite + jwt.key）持久化点
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
