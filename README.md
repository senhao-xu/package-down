# Package Proxy

一个轻量 Web 工具：用户在页面输入包名或 `.rpm` / `.deb` URL，服务端按目标操作系统解析仓库、下载系统包，并把文件流直接写入 ZIP 响应。

项目使用 Go 标准库实现。页面资源会打进二进制文件里，部署时只需要一个可执行文件。

## 功能

- 页面输入一个或多个包名，支持空格、逗号、换行分隔。
- 自动识别当前浏览器操作系统和 CPU 架构，并给出目标架构默认值。
- 支持选择目标操作系统：AlmaLinux 8/9、CentOS 7.9、Rocky Linux 9、CentOS Stream 9、Ubuntu 22.04/24.04。
- 支持 RPM 和 Ubuntu DEB 仓库。
- 默认递归下载依赖包，可在页面关闭。
- 支持直接粘贴 `.rpm` 或 `.deb` URL。
- RPM 通过仓库 `repodata` 解析包名、requires 和 provides。
- Ubuntu 通过 `Packages.gz` 解析包名、Depends 和 Pre-Depends。
- ZIP 边下载边生成，不先把包文件落盘。
- ZIP 内包含 `manifest.json`，记录来源、文件和错误信息。

## 本地启动

```bash
go run .
```

打开 http://localhost:3000 访问页面。

运行后服务会同时提供页面和接口：

- `/`：下载页面
- `/api/config`：当前仓库和系统配置
- `/download`：表单提交后返回 ZIP 下载流

## 编译成单个文件

Windows:

```bash
go build -o package-down.exe .
```

Linux:

```bash
go build -o package-down .
```

运行：

```bash
./package-down
```

页面、样式和脚本已经通过 Go `embed` 打进可执行文件，部署时不需要复制 `static` 目录。

## 发布 Release

推送 `v*` tag 会自动触发 GitHub Actions，编译并发布以下文件到 Releases：

- `package-down-linux-x86_64.tar.gz`
- `package-down-linux-arm64.tar.gz`
- `package-down-windows-x86_64.zip`
- `checksums.txt`

示例：

```bash
git tag v0.1.0
git push origin v0.1.0
```

也可以在 GitHub Actions 页面手动运行 `Release` workflow，并填写 tag。

## 快速部署

Linux 会自动识别 `x86_64` / `arm64` 架构，下载最新 Release、解压、授权并后台启动：

```bash
set -e

APP_DIR=/opt/package-down
REPO=senhao-xu/package-down
ARCH=$(uname -m)

case "$ARCH" in
  x86_64|amd64)
    ASSET=package-down-linux-x86_64.tar.gz
    ;;
  aarch64|arm64)
    ASSET=package-down-linux-arm64.tar.gz
    ;;
  *)
    echo "不支持的架构: $ARCH"
    exit 1
    ;;
esac

sudo mkdir -p "$APP_DIR"
cd "$APP_DIR"

sudo curl -fL -o package-down.tar.gz "https://github.com/${REPO}/releases/latest/download/${ASSET}"
sudo tar -xzf package-down.tar.gz
sudo chmod +x package-down
sudo nohup ./package-down > package-down.log 2>&1 &

echo "Package Proxy 已启动: http://localhost:3000"
echo "日志文件: ${APP_DIR}/package-down.log"
echo "查看加载进度: tail -f ${APP_DIR}/package-down.log"
```

调试时也可以前台启动，直接查看仓库加载进度：

```bash
cd /opt/package-down
./package-down
```

Windows PowerShell 下载并启动：

```powershell
$AppDir = "C:\package-down"
$Asset = "package-down-windows-x86_64.zip"
$Url = "https://github.com/senhao-xu/package-down/releases/latest/download/$Asset"

New-Item -ItemType Directory -Force -Path $AppDir | Out-Null
Set-Location $AppDir

Invoke-WebRequest -Uri $Url -OutFile $Asset
Expand-Archive -Force $Asset .
Start-Process -FilePath ".\package-down.exe"

Write-Host "Package Proxy 已启动: http://localhost:3000"
```

## 配置

可通过环境变量调整：

```bash
PORT=3000
REPO_URLS=https://repo.almalinux.org/almalinux/9/BaseOS/x86_64/os/,https://repo.almalinux.org/almalinux/9/AppStream/x86_64/os/
DEFAULT_OS_PROFILE=almalinux-9
REPO_ARCH=x86_64
MAX_PACKAGES=50
MAX_RESOLVED_PACKAGES=300
CACHE_TTL_MS=1800000
REQUEST_TIMEOUT_MS=120000
PRELOAD_REPOS=default
PRELOAD_TIMEOUT_MS=600000
ALLOW_DIRECT_URLS=true
```

`REPO_URLS` 用于自定义 RPM 仓库，必须指向包含 `repodata/repomd.xml` 的 RPM 仓库根路径。自定义仓库 URL 可以使用 `{arch}` 占位符，例如：

```bash
REPO_URLS=https://repo.example.com/os/{arch}/BaseOS/,https://repo.example.com/os/{arch}/AppStream/
```

默认端口是 `3000`，默认目标系统是 `AlmaLinux 9 / x86_64`。

启动时会先预加载仓库元数据，加载完成后才启动 Web 服务，避免第一次下载时浏览器长时间无响应：

- `PRELOAD_REPOS=default`：默认值，只预加载默认系统和默认架构。
- `PRELOAD_REPOS=all`：预加载所有内置系统和架构，启动更慢、占用内存更多。
- `PRELOAD_REPOS=none`：关闭启动预加载，恢复首次下载时按需加载。
- `PRELOAD_TIMEOUT_MS=600000`：预加载总超时时间，默认 10 分钟。

## 依赖下载说明

- RPM：使用 primary metadata 内的 `requires` / `provides` / 常见 file provides 做递归解析。
- Ubuntu：使用 `Depends` 和 `Pre-Depends` 做递归解析，版本约束会被忽略，优先选择仓库内可用的较新版本。
- 直接 URL 下载无法解析依赖，只会下载该 URL 指向的文件。
- 为避免依赖链过大，默认最多解析 `300` 个包，可通过 `MAX_RESOLVED_PACKAGES` 调整。
- 未预加载的系统仓库第一次使用时仍需要下载仓库元数据；后续会按 `CACHE_TTL_MS` 缓存。
