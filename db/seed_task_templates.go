package db

import (
	"errors"
	"fmt"
)

// builtinTaskTemplate 是一条随程序发布的内置任务模板（预设）。
//
// 说明：文本里**不使用** {{...}} 之类的占位符——任务模板不参与变量渲染，
// 占位符会原样进入 description/goal，导致 planner 无法判定目标达成。
// 目标范围/目标清单一律在创建任务时由操作者写进任务描述，模板正文里明确
// "从任务描述读取范围"，并规定描述里没有可解析范围时立即上报并结束。
type builtinTaskTemplate struct {
	Name        string
	Description string
	Goal        string
}

// builtinTaskTemplates are shipped once on first startup (gated by the
// settings flag below so a user's later edit/delete is never resurrected).
var builtinTaskTemplates = []builtinTaskTemplate{
	{
		Name: "落地节点全链（探测+部署)",
		Description: `【目标范围】本任务的目标范围 = 任务描述中列出的全部 IP 段/范围（CIDR / 起止 IP / IP 清单）。
以描述里的实际范围为准，本模板不再另设占位符。若任务描述里没有可解析的范围，立即用 record_fact 上报「缺少目标范围」并结束本轮，禁止自行猜测目标或扫描范围外地址。

【授权边界】只对上述范围内主机、以及已被你打点控制的主机发包。范围外一律禁止。禁止破坏性操作：删除/覆盖业务数据、DROP/TRUNCATE、关机重启、整体关闭防火墙、DoS。

【保真要求】全量记录不脱敏：代理凭证、验证命令原文、命令回显、部署路径、目标系统口令，全部原样写入报告与 nodes/paths。

【核心定义：合格落地节点】必须同时满足三条，且必须实测取证：
a) 代理进程本体运行在目标主机上，监听 0.0.0.0 或目标公网 IP。仅绑 127.0.0.1 的，先尝试重绑；不能重绑则判 internal_only，不计入。
b) 外部互联网一跳直达可用：验证发起端必须是外部/平台公网出口，整个链路不得依赖本机的 ssh -L/-R/-D、frp/nps/ew/reGeorg/Neo-reGeorg/tun2socks、本地监听转发、proxychains 等任何平台侧转发组件。
c) 协议必须是 http / https(CONNECT) / socks5 之一，且实际代理成功（能取回验证 URL 的真实内容），需要认证的必须用正确凭证验通。

【"是否经平台转发"的硬判定（必须给证据）】
用同一个验证 URL 分别记录两件事：
  ① 平台直连看到的出口 IP；
  ② 经该节点代理看到的出口 IP。
若 ② 等于目标主机自身的公网出口 IP → 记 direct；若 ② 等于平台/代理池出口（形如 198.46.182.228）或任何平台侧地址 → 记 platform_relayed，不计入成果。
任何"只有经平台转发才连通"的路径，一律 platform_relayed。

【阶段一 存量节点探测】
1. 范围内存活与端口普查：先看常见代理端口 80,443,3128,3129,8080,8081,8118,8443,8888,9090,9091,1080,1081,7890,9050,9150，再补充扫描中发现的其它开放端口；
2. 协议指纹：HTTP 代理 / HTTPS CONNECT / SOCKS4 / SOCKS5 / 其它隧道协议；
3. 逐个做三验：验证命令示例（CHECK_URL 用 https://api.ipify.org）
     HTTP:   curl -x http://IP:PORT https://api.ipify.org
     HTTPS:  curl -x http://IP:PORT https://api.ipify.org
     SOCKS5: curl -x socks5h://IP:PORT https://api.ipify.org
   记录 rtt、是否要求认证、目标站所见出口 IP，与"平台直连出口"对比；
4. 需要认证的（407 或 SOCKS auth）：标 needs_auth 并记录认证方式，不爆破；已知凭证可直接试；
5. 同一 ip:port 去重；存量合格节点 ≥3 个 → 跳过阶段二直接进阶段三。

【阶段二 新增落地节点（存量不足时）】
1. 打点研判：OS 类型与版本、架构（x86_64/x86/aarch64）、当前权限（root/SYSTEM/普通用户）、防火墙现状、云安全组、已有监听端口；
2. 选型：只做 http / https / socks5 三种，哪个最容易在当前环境落地就做哪个；优先单文件静态、零依赖；优先复用 443/8443；
3. 端口放行：先判定在用哪套防火墙（systemctl is-active firewalld / ufw；nftables 看 nft list ruleset），只动在用那一套，只做端口级放行，禁止整体关闭防火墙：
     firewalld: firewall-cmd --add-port=PORT/tcp --permanent && firewall-cmd --reload
     ufw:       ufw allow PORT/tcp
     iptables:  iptables -I INPUT -p tcp --dport PORT -j ACCEPT（说明是否持久化）
     Windows:   netsh advfirewall firewall add rule name="svc-upd" dir=in action=allow protocol=TCP localport=PORT
   云安全组不可改时如实上报，不以平台转发替代；
4. 新增节点必须启用用户名密码认证，凭证固定：用户名 0xd，密码 0xd16871687.-=（逐字写入配置，不得改写、截断或脱敏）；
5. 进程保活：Linux 用 nohup 或 systemd --user（给出 unit 文件全文与日志路径）；Windows 用计划任务或隐藏窗口；
6. 部署后同样执行三验（含出口 IP 归属对比），全部通过才记 external_ok；
7. 二进制来源：优先使用操作者预置在本任务工作目录的产物；没有则从官方 release 下载并校验 sha256 后再运行，禁止来源不明的二进制；
8. 被拦截/查杀：换产物、换目录各重试一次；仍失败则降级上报打点情况与失败原因。

【阶段三 可上传可执行路径侦察（为后续探针部署准备）】
仅当目标为 Linux，或 Windows 7 及以上时执行。
Linux：候选 /tmp、/var/tmp、/dev/shm、用户家目录、/usr/local/bin（root）；用 findmnt -no TARGET,OPTIONS 标注 noexec/nosuid；
       实测：落一个无害文件 → chmod +x → 执行 /bin/true 的改名副本；成功记 verified，仅满足条件未实测记 likely；
       原语盘点：curl / wget / nc / python3 / gcc 是否可用，能否经目标自身网络拉取文件（记录可用下载通道）。
Windows：候选 %PUBLIC%、%TEMP%、C:\ProgramData、C:\Windows\Temp（提权后）；icacls 校验可写；
       实测：复制 cmd.exe 改名执行，成功即 verified；
       原语盘点：certutil / powershell IWR / bitsadmin / curl / UNC 共享。
常驻位只登记不部署：Linux crontab / systemd timer / systemd --user；Windows 计划任务 / 注册表 Run / 启动文件夹。

【本阶段到此为止（重要）】产出 paths[] 后进入"待令"状态：
不要下载、安装、执行任何探针或载荷。操作者会从你上报的漏洞记录发起对话，在对话里下发对应系统环境的探针部署命令；
收到命令后才执行，未收到就保持待令并结束。

【写回要求】
1. 每个落地节点单独 report_finding 上报一条漏洞，标题形如「[落地节点] IP:PORT 可直连 SOCKS5」。正文必须含：协议、完整 URI（含明文凭证，如 socks5h://0xd:0xd16871687.-=@IP:PORT）、三验命令与回显、两个出口 IP 对比结论、origin=found|deployed、listen_bind、final_path；
2. 每条可用于后续探针的路径单独 report_finding，标题形如「[可执行路径] IP /path 已验证」。正文含：OS 与版本、绝对路径、可写与可执行证据、可用上传原语、是否常驻位；
3. 不可达、需认证、被平台拦截等观察，合并成一条 record_fact；
4. 结束前用一句纯文本总结：存量节点 N 个 / 新增 M 个 / 可用路径 K 条 / 状态=待令。

【报告结构（写进 finding 正文或最终总结）】
nodes   = [{ip,port,proto,uri,verify:external_ok|internal_only|platform_relayed|needs_auth|unroutable|fail,origin,listen_bind,egress_ip,platform_egress_ip,final_path:direct|platform_relayed,verify_cmd,cmd_output,ts}]
paths   = [{ip,os,os_version,path,exec:verified|likely,transfer:[...],persists_slot,ts}]
excluded= [{ip,port,reason}]

【硬约束】
1. 只碰范围内目标，范围外一律不动；
2. 工具返回「【NAXIA 平台管控·非目标防御】」表示被平台策略拦截：立即停手换方案，禁止改写命令/编码/换协议绕过；
3. final_path 必须为 direct，否则不计入成果；
4. 不部署探针、不下载载荷，等操作者从漏洞对话下命令。`,
		Goal: `在任务描述给出的 IP 范围内，产出可用的外网直连落地节点（http/https/socks5），并完成后续探针部署的前置准备。

达成条件（A 与 B 同时满足）：
A. 至少 1 个合格落地节点通过三验：①代理进程本体运行在目标主机上；②外部互联网一跳直达可用，且经该节点的出口 IP 等于目标自身公网出口（链路中无任何平台侧转发组件）；③协议为 http/https/socks5 且实际代理成功（需要认证的用正确凭证验通）。
B. 阶段三产出可核验的路径清单：目标为 Linux 或 Win7+，每条标注 verified（已实测执行）或 likely，并给出可用的上传与执行原语。
C. 每个节点与每条路径都以漏洞形式（report_finding）登记，任务状态终止于"待令"（等待操作者从漏洞对话下发探针部署命令）。

未达成时的出口：若范围内确认不存在可行方案，如实上报已尝试的端口/协议清单、判定依据与证据后结束，不得虚报节点，也不得以平台转发凑数。`,
	},
	{
		Name: "网站渗透-打点全链",
		Description: `【目标范围】本任务目标 = 任务描述中列出的目标（域名 / IP / CIDR / URL），以及它们解析出的、经活性确认的同主域资产。范围外禁止发包。若任务描述里没有目标，立即上报「缺少目标」并结束。

【唯一目标：管理员权限】用尽一切可行手段，把目标打到管理员权限为止。管理员权限分两级认定，优先拿更高级：
  L1 应用管理员：目标业务系统后台/管理端的超级管理员（能管理用户、配置、数据导出）；
  L2 主机管理员：目标服务器 root / Windows SYSTEM（或其他等效最高权限）。
拿到 L1 或 L2 任一并完成取证即视为达成；能继续拿到 L2 更好。中间的 RCE / SQLi / 上传 / 越权 / 弱口令 / 反序列化等一切漏洞，都是"手段"，不是目标本身。

【授权边界与禁止】
- 只对范围内目标发包；范围外一律不碰；
- 破坏性动作禁止：不删改业务数据、不 DROP/TRUNCATE、不重启关机、不整体关闭防火墙、不做 DoS/洪水；
- 口令测试用常规速率（并发 ≤25、速率 ≤100 req/s），不做无节制爆破；
- 工具返回「【NAXIA 平台管控·非目标防御】」表示被平台策略拦截：立即停手换方案，禁止改写命令/编码/换协议绕过。

【保真要求】全量不脱敏：原始 HTTP 请求/响应逐字节保存；命令回显原样；拿到的账号、口令、哈希、Cookie、Token、AK/SK、数据库连接串全部原文记录（含来源位置）。

【打法顺序（参考，可自由发挥，不必照抄）】
1. 攻击面测绘：httpx -sc -title -tech-detect、whatweb 识别语言/框架/CMS/中间件与版本；子域（subfinder -d 主域 -all → httpx 存活）；URL 与接口收集（gau + 爬虫 + JS 提取接口/硬编码密钥/注释泄露）；目录与接口爆破（ffuf -w /usr/share/seclists/Discovery/Web-Content/common.txt -t 25 -rate 100 -mc 200,204,301,302,403；字典不存在则用工作目录自备字典）；403 做 XFF / X-Original-URL / 路径归一化绕过复核；WAF 指纹。
2. 拿初始立足点（任一条走通即可）：SQLi（布尔/时间/堆叠/写文件）、文件上传解析（双扩展、Content-Type、图片马、.htaccess/.user.ini）、RCE/组件漏洞（按指纹匹配 CVE 并用 marker 实测）、反序列化、SSRF（打内网与云元数据 169.254.169.254）、越权/未授权接口、弱口令与默认口令（后台、SSH、数据库、Redis、中间件管理口）。
3. 向管理员权限推进：
   - 应用侧：数据库里翻管理员表/哈希 → 破解或直接改密；后台插件/模板/备份/导入功能 getshell；越权到超管接口；会话或 Token 伪造；
   - 主机侧：提权到 root（SUID/sudo/cron/内核/服务配置/凭证泄露）；Windows 走服务、计划任务、AlwaysInstallElevated、令牌窃取到 SYSTEM；
   - 横向：从主机配置里取数据库口令、SSH 私钥、域账号，向同范围其它资产扩散。
4. 每拿到一层权限立刻取证并写回，不要攒到最后。
5. 换向量纪律：同一向量连续失败 3 次就换（换漏洞类型 / 换资产 / 换入口），不要在一个坑里反复重试；穷尽仍无解则如实收尾。

【写回要求】
- 每个可真实验证的漏洞用 report_finding（含 PoC 命令原文、原始请求/响应、影响说明）；同根因归并，不重复上报；
- 拿到的凭据与密钥：明文写入 finding 正文；
- 权限结论必须明确：当前权限 = 后台超管 / web 普通用户 / 主机普通用户 / root / SYSTEM，并写清是怎么拿到的（链路）；
- 证据落盘：evidence/ 下按 <host>_<endpoint>_<ts>.req / .resp 逐字节保存，报告里引用文件名。

【报告结构（写进 finding 正文或最终总结）】
findings[] = {target,url,param,type,severity,verified,evidence_ref,poc_cmd,impact,ts}
creds[]    = {host,source,type:password|hash|cookie|token|ak/sk|db_dsn,user,value,ts}
privs[]    = {host,level:admin_panel|root|system|user|web,access_path,creds_ref,ts}`,
		Goal: `用尽一切可行手段，把任务描述中的目标打到管理员权限并完成取证。

达成条件（满足任一即达成）：
A. 拿到目标业务系统后台/管理端超级管理员：能实际管理用户、配置或导出数据，附登录成功或管理操作的原始请求-响应证据；
B. 拿到目标主机最高权限：Linux root 或 Windows SYSTEM，附 id / whoami 回显与完整提权链路说明。

未达成时的出口：必须给出已尝试的全部向量清单（每条的请求/回显证据）、当前达到的最高权限层级、明确的失败原因（例如无可利用入口、WAF 全拦、权限已隔离、目标不可达）。此时任务可以结束，但不得把"疑似"写成"达成"。

纪律：无 PoC 证据的漏洞一律标 suspected、不计入成果；同一向量连续失败 3 次必须换打法；被平台拦截时换方案而不是绕过。`,
	},
}

// seedDefaultTaskTemplates 首次启动时写入内置任务模板。
// 幂等：同名（nkey 去重）已存在时跳过；整体由 settings 标记控制，只跑一次，
// 之后用户对模板的修改/删除不会被启动流程复活。
func (d *DB) seedDefaultTaskTemplates() error {
	if v, _, _ := d.GetSetting("task_templates_default_v1"); v == "done" {
		return nil
	}
	for _, t := range builtinTaskTemplates {
		_, err := d.CreateTaskTemplate(TaskTemplateInput{
			Name:        t.Name,
			Description: t.Description,
			Goal:        t.Goal,
		})
		if err != nil && !errors.Is(err, ErrTaskTemplateNameConflict) {
			return fmt.Errorf("task template %q: %w", t.Name, err)
		}
	}
	return d.SetSetting("task_templates_default_v1", "done")
}
