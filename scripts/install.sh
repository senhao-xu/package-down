#!/bin/sh
set -eu

APP_DIR="${APP_DIR:-/opt/package-down}"
REPO="${REPO:-senhao-xu/package-down}"
PORT="${PORT:-3000}"

case "$(uname -m)" in
	x86_64|amd64)
		ASSET="package-down-linux-x86_64.tar.gz"
		;;
	aarch64|arm64)
		ASSET="package-down-linux-arm64.tar.gz"
		;;
	*)
		echo "不支持的架构: $(uname -m)" >&2
		exit 1
		;;
esac

if ! command -v curl >/dev/null 2>&1; then
	echo "需要先安装 curl" >&2
	exit 1
fi

TMP_DIR="$(mktemp -d)"
cleanup() {
	rm -rf "$TMP_DIR"
}
trap cleanup EXIT

URL="https://github.com/${REPO}/releases/latest/download/${ASSET}"

mkdir -p "$APP_DIR"
curl -fL "$URL" -o "$TMP_DIR/package-down.tar.gz"
tar -xzf "$TMP_DIR/package-down.tar.gz" -C "$TMP_DIR"
install -m 0755 "$TMP_DIR/package-down" "$APP_DIR/package-down"

if command -v systemctl >/dev/null 2>&1; then
	cat > /etc/systemd/system/package-down.service <<EOF
[Unit]
Description=Package Proxy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Environment=PORT=${PORT}
WorkingDirectory=${APP_DIR}
ExecStart=${APP_DIR}/package-down
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
	systemctl daemon-reload
	systemctl enable --now package-down
	echo "Package Proxy 已安装并启动: http://localhost:${PORT}"
	echo "查看日志: journalctl -u package-down -f"
else
	nohup "$APP_DIR/package-down" > "$APP_DIR/package-down.log" 2>&1 &
	echo "Package Proxy 已安装并后台启动: http://localhost:${PORT}"
	echo "日志文件: ${APP_DIR}/package-down.log"
fi
