#!/usr/bin/env python3
"""Root-owned host worker. No shell commands or host paths come from HTTP input.

The only writable API is POST /apply {version: vX.Y.Z}. Run under systemd;
mount only its socket directory (read-only) into the application container.
"""
import argparse
import base64
import fcntl
import hashlib
import hmac
import http.server
import json
import os
from pathlib import Path
import re
import shutil
import socket
import stat
import subprocess
import threading
import time
import uuid
from urllib.parse import urlparse, unquote


TAG = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\Z")
IMAGE = re.compile(r"(?:(?:ghcr\.io|127\.0\.0\.1:[0-9]+)/)?[a-z0-9][a-z0-9_-]*/[a-z0-9]+(?:[._-][a-z0-9]+)*\Z")


def version(tag):
    if not isinstance(tag, str) or not TAG.fullmatch(tag):
        raise ValueError("正式版本必须为 vX.Y.Z")
    return tuple(map(int, tag[1:].split(".")))


def atomic_json(path, value):
    temp = path.with_name(path.name + ".tmp")
    with open(temp, "w", encoding="utf-8") as out:
        json.dump(value, out, ensure_ascii=False)
        out.flush()
        os.fsync(out.fileno())
    os.replace(temp, path)
    fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


class Worker:
    def __init__(self, project, state, image, health_seconds=30):
        self.project, self.state, self.image = Path(project), Path(state), image
        if not IMAGE.fullmatch(image):
            raise ValueError("镜像仓库格式无效")
        self.state.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.lock = threading.Lock()
        self.health_seconds = health_seconds
        # A second worker must never operate on the same deployment.
        self.process_lock = open(self.state / "worker.lock", "w")
        fcntl.flock(self.process_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        self.status_path = self.state / "status.json"
        self.status = json.loads(self.status_path.read_text()) if self.status_path.exists() else {}
        if self.status.get("running"):
            self.status.update(running=False, phase="failed", error="宿主机更新服务曾中断；请按记录检查备份和部署，禁止自动重试")
            self.status["recovery_required"] = True
        self.status.setdefault("phase", "idle")
        self.status.setdefault("running", False)
        self.status["ready"] = not self.status.get("recovery_required", False)
        atomic_json(self.status_path, self.status)

    def run(self, *args, timeout=600, input=None):
        p = subprocess.run(args, cwd=self.project, input=input, stdout=subprocess.PIPE,
                           stderr=subprocess.PIPE, timeout=timeout, check=False)
        if p.returncode:
            # Never return Docker/Compose stderr to the browser: it can contain secrets.
            raise RuntimeError("命令失败: " + " ".join(args[:2]) + "，请在宿主机检查服务日志")
        return p.stdout

    def compose(self, *args, **kwargs):
        return self.run("docker", "compose", *args, **kwargs)

    def container(self):
        ids = self.compose("ps", "-a", "-q", "artex").decode().split()
        if len(ids) != 1:
            raise RuntimeError("需要且仅支持一个 artex 容器")
        return json.loads(self.run("docker", "inspect", ids[0]))[0]

    def emit(self, phase, message, **values):
        with self.lock:
            self.status.update(phase=phase, message=message, **values)
            atomic_json(self.status_path, self.status)

    def snapshot(self):
        with self.lock:
            return dict(self.status)

    def submit(self, tag):
        version(tag)
        with self.lock:
            if not self.status["ready"] or self.status["running"]:
                raise ValueError("更新正在进行或上次中断尚需人工恢复")
            self.status = dict(ready=True, running=True, phase="downloading", version=tag,
                               message="正在检查部署并拉取新镜像", error="")
            atomic_json(self.status_path, self.status)
        threading.Thread(target=self.upgrade, args=(tag,), daemon=True).start()

    def probe(self, container):
        # Probes bypass public Basic Auth/TLS without exposing management ports.
        code = """import urllib.request,json,re,sys
base='http://127.0.0.1:8787'
headers=json.loads(sys.stdin.read())
def get(p):
    return urllib.request.urlopen(urllib.request.Request(base+p,headers=headers),timeout=5).read()
h=json.loads(get('/api/health')); a=json.loads(get('/api/auth/status'))
get('/api/update/settings')
html=get('/').decode()
assert '<!doctype html' in html.lower()
assets=set(re.findall(r'(?:src|href)=[\"\\\']([^\"\\\']+\\.(?:js|css)(?:\\?[^\"\\\']*)?)[\"\\\']',html))
assert assets
for p in assets:
    assert p.startswith('/') and get(p)
assert h.get('ok') is True
print(json.dumps({'version':h['version'],'initialized':a['initialized']}))
"""
        return json.loads(self.run("docker", "exec", "-i", container["Id"], "python3", "-c", code,
                                   input=json.dumps(self.probe_headers).encode(), timeout=30))

    @staticmethod
    def token(key, claims):
        def enc(value):
            return base64.urlsafe_b64encode(json.dumps(value, separators=(",", ":")).encode()).rstrip(b"=")
        payload = enc({"alg": "HS256", "typ": "JWT"}) + b"." + enc(claims)
        return (payload + b"." + base64.urlsafe_b64encode(hmac.new(key, payload, hashlib.sha256).digest()).rstrip(b"=")).decode()

    def auth_headers(self, key):
        expiry = int(time.time()) + 7200
        headers = {"Authorization": "Bearer " + self.token(key, {"sub": "ARTEX", "exp": expiry})}
        raw = self.db("psql", "-At", "-c", "SELECT value FROM settings WHERE key='auth.http_basic';").strip()
        cfg = json.loads(raw) if raw else {}
        if cfg.get("enabled"):
            gate_key = hmac.new(key, b"artex/http-basic-auth/session/v1", hashlib.sha256).digest()
            cookie = self.token(gate_key, {"sub": cfg["revision"], "aud": ["artex-basic-auth"], "exp": expiry})
            headers["Cookie"] = "artex_basic_auth=" + cookie
        return headers

    def db(self, program, *args, input=None):
        # Arguments and shell text are constants, never interpolated from a request.
        script = 'exec "$@" -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
        return self.compose("exec", "-T", "postgres", "sh", "-c", script, "updater", program, *args, input=input)

    def mounts(self, container):
        result = []
        for m in container["Mounts"]:
            if not m["RW"] or m["Destination"] == "/run/naxia-updater":
                continue
            p = Path(m["Source"])
            if m["Type"] != "bind" or p.is_symlink() or not (p.is_dir() or p.is_file()):
                raise RuntimeError("应用可写挂载必须为普通 bind 目录或文件")
            p = p.resolve()
            # Backups cannot overlap application mounts or deployment control files.
            for protected in (self.state.resolve(), self.project.resolve()):
                if p == protected or p in protected.parents or (protected == self.state.resolve() and protected in p.parents):
                    raise RuntimeError("挂载与更新控制目录重叠")
            result.append((str(p), m["Destination"]))
        sources = [Path(x[0]) for x in result]
        if any(a != b and a in b.parents for a in sources for b in sources):
            raise RuntimeError("不支持互相嵌套的可写挂载")
        return sorted(result)

    @staticmethod
    def inventory(root):
        root = Path(root)
        entries = [root] + (sorted(root.rglob("*")) if root.is_dir() else [])
        result = {}
        for entry in entries:
            name = str(entry.relative_to(root))
            if entry.is_symlink():
                result[name] = ["link", os.readlink(entry)]
            elif entry.is_file():
                with open(entry, "rb") as source:
                    h = hashlib.sha256()
                    for block in iter(lambda: source.read(1024 * 1024), b""):
                        h.update(block)
                    digest = h.hexdigest()
                st = entry.stat()
                result[name] = ["file", st.st_size, digest, st.st_mode, st.st_uid, st.st_gid]
            elif entry.is_dir():
                st = entry.stat()
                result[name] = ["dir", st.st_mode, st.st_uid, st.st_gid]
            else:
                raise RuntimeError("可写目录包含 socket/FIFO/设备等不能完整备份的条目")
        return result

    @staticmethod
    def preserve_owners(src, dst):
        src, dst = Path(src), Path(dst)
        entries = [src] + (list(src.rglob("*")) if src.is_dir() else [])
        for entry in entries:
            st = entry.lstat()
            os.chown(dst / entry.relative_to(src) if src.is_dir() else dst,
                     st.st_uid, st.st_gid, follow_symlinks=False)
            if not entry.is_symlink():
                os.chmod(dst / entry.relative_to(src) if src.is_dir() else dst, stat.S_IMODE(st.st_mode))

    def backup_mounts(self, mounts, root):
        for i, (src, _) in enumerate(mounts):
            target = root / str(i)
            if Path(src).is_dir():
                shutil.copytree(src, target, symlinks=True)
            else:
                shutil.copy2(src, target)
            self.preserve_owners(src, target)
            if self.inventory(src) != self.inventory(target):
                raise RuntimeError("挂载备份完整性校验失败")

    def check_space(self, mounts):
        # st_blocks includes sparse-file allocation; copying can expand holes, so
        # reserve logical size + per-entry overhead + database size + headroom.
        needed = 0
        for src, _ in mounts:
            p = Path(src)
            entries = [p] if p.is_file() else p.rglob("*")
            for entry in entries:
                st = entry.lstat()
                needed += (st.st_size if entry.is_file() and not entry.is_symlink() else 0) + 8192
        sql = "SELECT pg_database_size(current_database());"
        db_size = int(self.db("psql", "-At", "-c", sql).strip())
        needed = int((needed + db_size) * 1.2) + 256 * 1024**2
        if shutil.disk_usage(self.state).free < needed:
            raise RuntimeError("备份盘空间不足（含数据库、所有可写挂载、文件开销和余量）")

    def restore_mounts(self, mounts, root):
        for i, (src, _) in enumerate(mounts):
            p, saved = Path(src), root / str(i)
            if p.is_symlink():
                raise RuntimeError("恢复目标变成符号链接，已停止")
            if p.is_dir():
                shutil.rmtree(p)
                shutil.copytree(saved, p, symlinks=True)
            else:
                shutil.copy2(saved, p)
            self.preserve_owners(saved, p)
            if self.inventory(p) != self.inventory(saved):
                raise RuntimeError("挂载恢复完整性校验失败")

    @staticmethod
    def file_hash(path):
        digest = hashlib.sha256()
        with open(path, "rb") as source:
            for block in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(block)
        return digest.hexdigest()

    def verify_backup(self, mounts, root):
        manifest = json.loads((root / "complete.json").read_text())
        if self.file_hash(root / "database.dump") != manifest["database_sha256"]:
            raise RuntimeError("数据库恢复点校验失败，未覆盖现有数据")
        for i, _ in enumerate(mounts):
            if self.inventory(root / str(i)) != manifest["mounts"][str(i)]:
                raise RuntimeError("文件恢复点校验失败，未覆盖现有数据")

    @staticmethod
    def validate_database(cfg):
        app = cfg["services"]["artex"].get("environment", {})
        pg = cfg["services"]["postgres"].get("environment", {})
        u = urlparse(app.get("ARTEX_PG_DSN", ""))
        if (u.scheme not in ("postgres", "postgresql") or u.hostname != "postgres" or
                u.port not in (None, 5432) or unquote(u.username or "") != pg.get("POSTGRES_USER") or
                unquote(u.password or "") != pg.get("POSTGRES_PASSWORD") or
                unquote(u.path.lstrip("/")) != pg.get("POSTGRES_DB")):
            raise RuntimeError("自动升级仅支持应用实际连接本 Compose postgres 的部署，外部数据库需独立升级方案")

    def restore_database(self, dump):
        # No application container is running. Terminate other sessions before
        # replacing the entire database, including objects added by migrations.
        self.db("psql", "-v", "ON_ERROR_STOP=1", "-c",
                "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid();")
        with open(dump, "rb") as source:
            p = subprocess.run(["docker", "compose", "exec", "-T", "postgres", "sh", "-c",
                'exec pg_restore --create --clean --if-exists --exit-on-error -U "$POSTGRES_USER" -d template1'],
                cwd=self.project, stdin=source, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=600)
            if p.returncode: raise RuntimeError("数据库恢复失败")

    def write_env(self, content):
        p = self.project / ".env"
        tmp = p.with_name(".env.updater-tmp")
        with open(tmp, "w", encoding="utf-8") as out:
            os.chmod(tmp, 0o600)
            out.write(content)
            out.flush()
            os.fsync(out.fileno())
        os.replace(tmp, p)

    @staticmethod
    def pin_env(content, image, tag, digest):
        lines = [x for x in content.splitlines() if not re.match(r"\s*(?:export\s+)?ARTEX_(?:IMAGE|TAG)\s*=", x)]
        return "\n".join(lines + ["ARTEX_IMAGE=" + image, "ARTEX_TAG=" + tag + "@" + digest, ""])

    def upgrade(self, tag):
        backup = self.state / ("backup-" + uuid.uuid4().hex)
        old_env = None
        stopped = False
        backed_up = False
        switched = False
        old = None
        try:
            old = self.container()
            cfg = json.loads(self.compose("config", "--format", "json"))
            self.validate_database(cfg)
            key_mount = [m for m in old["Mounts"] if m["Destination"] == "/app/jwt.key" and m["Type"] == "bind"]
            if len(key_mount) != 1 or not Path(key_mount[0]["Source"]).is_file():
                raise RuntimeError("必须先将 /app/jwt.key 持久化为宿主机 bind 文件")
            key_path = Path(key_mount[0]["Source"])
            key_hash = hashlib.sha256(key_path.read_bytes()).hexdigest()
            self.probe_headers = self.auth_headers(key_path.read_bytes().strip())
            before = self.probe(old)
            old_image = json.loads(self.run("docker", "image", "inspect", old["Image"]))[0]
            old_labels = old_image.get("Config", {}).get("Labels") or {}
            if old_labels.get("org.opencontainers.image.version", "").removeprefix("v") != before["version"].removeprefix("v"):
                raise RuntimeError("当前程序与镜像版本不一致，请先手动统一镜像，确保失败后能恢复原版本")
            if version(tag) <= version("v" + before["version"].removeprefix("v")):
                raise RuntimeError("目标版本必须高于当前运行版本")
            old_mounts = self.mounts(old)
            env_path = self.project / ".env"
            if env_path.is_symlink() or not env_path.is_file():
                raise RuntimeError("需要普通 .env 部署配置文件")
            old_env = env_path.read_text()
            if cfg["services"]["artex"]["image"] != old["Config"]["Image"]:
                raise RuntimeError("部署配置与当前镜像不一致，请先修正后再更新")
            # Pull an exact version, then pin its immutable repository digest.
            target = self.image + ":" + tag
            self.run("docker", "pull", target, timeout=1800)
            image = json.loads(self.run("docker", "image", "inspect", target))[0]
            digests = [x for x in image.get("RepoDigests", []) if x.startswith(self.image + "@sha256:")]
            if len(digests) != 1:
                raise RuntimeError("无法确定目标镜像摘要")
            digest = digests[0].split("@", 1)[1]
            self.run("docker", "run", "--rm", "--network", "none", "--entrypoint", "/app/artex", digests[0], "-h")
            labels = image.get("Config", {}).get("Labels") or {}
            if labels.get("org.opencontainers.image.version") != tag:
                raise RuntimeError("镜像版本标签与请求版本不一致")
            self.check_space(old_mounts)
            # Stop before snapshotting: database and all writable app mounts are a
            # single recovery point. PostgreSQL itself is never recreated.
            self.emit("verifying", "正在停止应用并备份数据库和全部可写挂载", backup=str(backup))
            stopped = True
            self.compose("stop", "-t", "60", "artex")
            self.assert_stopped()
            self.check_space(old_mounts)
            backup.mkdir(mode=0o700)
            atomic_json(backup / "recovery.json", {"mounts": old_mounts, "image": old["Config"]["Image"], "image_id": old["Image"], "before": before})
            (backup / "env").write_text(old_env)
            # Stream pg_dump to disk; never buffer production databases in memory.
            with open(backup / "database.dump", "wb") as output:
                proc = subprocess.run(["docker", "compose", "exec", "-T", "postgres", "sh", "-c",
                    'exec pg_dump -Fc -U "$POSTGRES_USER" -d "$POSTGRES_DB"'],
                    cwd=self.project, stdout=output, stderr=subprocess.PIPE, timeout=600)
                if proc.returncode: raise RuntimeError("数据库备份失败")
            with open(backup / "database.dump", "rb") as source:
                listing = subprocess.run(["docker", "compose", "exec", "-T", "postgres", "pg_restore", "--list"],
                    cwd=self.project, stdin=source, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=600)
                if listing.returncode: raise RuntimeError("数据库备份校验失败")
            self.backup_mounts(old_mounts, backup)
            backed_up = True
            os.sync()
            atomic_json(backup / "complete.json", {"complete": True,
                "database_sha256": self.file_hash(backup / "database.dump"),
                "mounts": {str(i): self.inventory(backup / str(i)) for i, _ in enumerate(old_mounts)}})
            self.write_env(self.pin_env(old_env, self.image, tag, digest))
            new_cfg = json.loads(self.compose("config", "--format", "json"))
            if new_cfg["services"]["artex"]["image"] != target + "@" + digest:
                raise RuntimeError("Compose 的 image 未引用 ARTEX_IMAGE / ARTEX_TAG，已拒绝升级")
            # Everything except artex.image must be identical (including pg settings).
            new_cfg["services"]["artex"]["image"] = cfg["services"]["artex"]["image"]
            if new_cfg != cfg:
                raise RuntimeError("镜像更新意外改变其他 Compose 配置")
            self.emit("staged", "正在重建应用容器并验收新版镜像")
            switched = True
            self.compose("up", "-d", "--no-deps", "--force-recreate", "artex")
            new = self.wait_healthy(tag, before, old_mounts, image["Id"])
            if hashlib.sha256(key_path.read_bytes()).hexdigest() != key_hash:
                raise RuntimeError("会话签名密钥发生变化")
            self.emit("verifying", "新版验收通过，正在清理本次旧镜像与备份")
            # From here the new deployment is committed. Cleanup failure must
            # never trigger database rollback after the old image was removed.
            stopped = False
            # Remove only this upgrade's previous image, never prune unrelated images.
            # Docker refuses removal when another container still references it.
            warning = ""
            if old["Image"] != new["Image"]:
                try:
                    users = self.run("docker", "ps", "-a", "-q", "--filter", "ancestor=" + old["Image"]).strip()
                    if users: raise RuntimeError("旧镜像仍在使用")
                    tags = [x for x in old_image.get("RepoTags", []) if x.startswith(self.image + ":")]
                    for ref in tags or [old["Image"]]:
                        self.run("docker", "image", "rm", ref)
                except Exception:
                    warning = "；旧镜像仍被引用或有多个标签，已保留，请人工检查"
            try:
                shutil.rmtree(backup)
            except Exception:
                warning += "；本次备份清理未完成，已保留剩余文件"
            self.emit("complete", "镜像升级和验收完成，本次备份已清理" + warning, running=False)
        except Exception as exc:
            rollback_error = ""
            if stopped:
                try:
                    self.compose("stop", "-t", "60", "artex")
                    self.assert_stopped()
                    if old_env is not None:
                        self.write_env(old_env)
                    if backed_up and switched:
                        self.verify_backup(old_mounts, backup)
                        # Remove the new app before replacing bind-mount sources.
                        self.compose("rm", "-f", "artex")
                        self.restore_mounts(old_mounts, backup)
                        self.restore_database(backup / "database.dump")
                    self.compose("up", "-d", "--no-deps", "artex")
                    self.wait_healthy("v" + before["version"].removeprefix("v"), before, old_mounts, old["Image"])
                except Exception:
                    rollback_error = "；自动恢复未完成，请按宿主机备份恢复，禁止重试"
            self.emit("failed", "升级失败，已有恢复材料已保留", error=str(exc) + rollback_error,
                      running=False, ready=not bool(rollback_error), recovery_required=bool(rollback_error), backup=str(backup))

    def assert_stopped(self):
        if self.container()["State"]["Running"]:
            raise RuntimeError("应用仍在运行，拒绝备份或恢复")

    def recover(self, path):
        """Explicit root-only recovery after worker/power interruption.

        Never infer that an incomplete backup is safe, or replay automatically
        against a newly started deployment. Operator selects the recovery point.
        """
        root = Path(path).resolve()
        if root.parent != self.state.resolve() or not root.name.startswith("backup-") or not (root / "complete.json").is_file():
            raise RuntimeError("恢复点不在本服务目录或备份未完整完成")
        record = json.loads((root / "recovery.json").read_text())
        self.verify_backup(record["mounts"], root)
        self.compose("stop", "-t", "60", "artex")
        ids = self.compose("ps", "-a", "-q", "artex").decode().split()
        if ids:
            self.assert_stopped()
        self.write_env((root / "env").read_text())
        self.compose("rm", "-f", "artex")
        self.restore_mounts(record["mounts"], root)
        self.restore_database(root / "database.dump")
        self.compose("up", "-d", "--no-deps", "artex")
        c = self.container()
        key = next(m["Source"] for m in c["Mounts"] if m["Destination"] == "/app/jwt.key")
        self.probe_headers = self.auth_headers(Path(key).read_bytes().strip())
        self.wait_healthy("v" + record["before"]["version"].removeprefix("v"), record["before"],
                          [tuple(m) for m in record["mounts"]], record["image_id"])
        self.emit("failed", "已从选定恢复点恢复，备份继续保留", ready=True, running=False,
                  recovery_required=False, error="上次镜像升级未完成，已恢复旧版")

    def wait_healthy(self, tag, before, mounts, image_id):
        deadline, stable = time.monotonic() + 180, None
        while time.monotonic() < deadline:
            try:
                c = self.container()
                if c["Image"] != image_id or self.mounts(c) != mounts or c["RestartCount"] or c["State"].get("OOMKilled"):
                    raise RuntimeError("容器镜像、挂载或运行状态不符合预期")
                probe = self.probe(c)
                if probe != {"version": tag.removeprefix("v"), "initialized": before["initialized"]}:
                    raise RuntimeError("版本或登录初始化状态不符")
                stable = stable or time.monotonic()
                if time.monotonic() - stable >= self.health_seconds:
                    return c
            except Exception:
                stable = None
            time.sleep(2)
        raise RuntimeError("新版容器连续健康检查未通过")


class UnixHTTPServer(http.server.ThreadingHTTPServer):
    address_family = socket.AF_UNIX

    def server_bind(self):
        self.socket.bind(self.server_address)
        self.server_name, self.server_port = "localhost", 0


class Handler(http.server.BaseHTTPRequestHandler):
    def reply(self, code, value):
        data = json.dumps(value, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self.reply(200, self.server.worker.snapshot()) if self.path == "/status" else self.reply(404, {"error": "not found"})

    def do_POST(self):
        try:
            n = int(self.headers.get("Content-Length", "0"))
            if self.path != "/apply" or not 0 < n <= 1024:
                raise ValueError("无效的更新请求")
            value = json.loads(self.rfile.read(n))
            if not isinstance(value, dict) or set(value) != {"version"}:
                raise ValueError("只允许指定正式版本")
            self.server.worker.submit(value["version"])
            self.reply(202, self.server.worker.snapshot())
        except (ValueError, KeyError, TypeError):
            self.reply(409, {"error": "更新请求无效、正在更新或需要人工恢复"})

    def log_message(self, *_):
        pass


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--project", required=True)
    p.add_argument("--state", default="/var/lib/naxia-updater")
    p.add_argument("--socket", default="/run/naxia-updater/control.sock")
    p.add_argument("--image", default="ghcr.io/jishengkele/naxia")
    p.add_argument("--recover", help="选定完整备份恢复；先停止 systemd 更新服务")
    a = p.parse_args()
    os.umask(0o077)
    worker = Worker(a.project, a.state, a.image)
    if a.recover:
        worker.recover(a.recover)
        return
    path = Path(a.socket)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.unlink(missing_ok=True)
    server = UnixHTTPServer(str(path), Handler)
    server.worker = worker
    os.chmod(path, 0o600)
    server.serve_forever()


if __name__ == "__main__":
    main()
