#!/bin/sh
# Install only the host service. Compose bridge activation is a separate,
# explicit step so existing deployment overrides/ports are never overwritten.
set -eu
[ "$(id -u)" = 0 ] || { echo '请以 root 执行' >&2; exit 1; }
project=${1:?用法: sh deploy/install-docker-updater.sh /opt/naxia [镜像仓库]}
image=${2:-ghcr.io/jishengkele/naxia}
case "$project" in /*) ;; *) echo '项目目录必须为绝对路径' >&2; exit 1 ;; esac
case "$project$image" in *[!a-zA-Z0-9_./-]*) echo '目录或镜像包含不支持的字符' >&2; exit 1 ;; esac
[ -f "$project/.env" ] || { echo '项目目录缺少 .env' >&2; exit 1; }
command -v python3 >/dev/null
docker compose version >/dev/null
base=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
install -d -m 700 /var/lib/naxia-updater /usr/local/lib/naxia-updater
install -m 700 "$base/docker_updater.py" /usr/local/lib/naxia-updater/docker_updater.py
cat > /etc/systemd/system/naxia-updater.service <<EOF
[Unit]
Description=Naxia Docker image updater
Requires=docker.service
After=docker.service

[Service]
Type=simple
User=root
UMask=0077
RuntimeDirectory=naxia-updater
RuntimeDirectoryMode=0700
RuntimeDirectoryPreserve=yes
ExecStart=/usr/bin/python3 /usr/local/lib/naxia-updater/docker_updater.py --project $project --image $image
Restart=on-failure
RestartSec=5
Nice=10
CPUQuota=50%
IOWeight=20

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now naxia-updater
echo '宿主机服务已安装。请按 README 合并 docker-compose.updater.yml 并重建一次应用，启用面板完整升级。'
