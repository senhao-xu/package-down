# Package Proxy

一个轻量 Web 工具：用户在页面输入包名或 `.rpm` / `.deb` URL，服务端按目标操作系统解析仓库、下载系统包，并把文件流直接写入 ZIP 响应。

项目使用 Go 标准库实现。页面资源会打进二进制文件里，部署时可以使用单个可执行文件，也可以使用 Docker 容器。

## 功能

- 页面输入一个或多个包名，支持空格、逗号、换行分隔。
- 自动识别当前浏览器操作系统和 CPU 架构，并给出目标架构默认值。
- 支持选择目标操作系统：Ubuntu 20.04/22.04/24.04、CentOS 7.9、CentOS Stream 9、AlmaLinux 8/9、Rocky Linux 9。
- 支持 RPM 和 Ubuntu DEB 仓库。
- 是否下载依赖由用户选择，默认递归下载依赖包，可在页面关闭。
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

## Docker 部署

直接从源码构建镜像并启动：

```bash
docker build -t package-down . && docker run -d --name package-down -p 3000:3000 --restart unless-stopped package-down
```

如需传入配置，可在 `docker run` 时追加环境变量：

```bash
docker run -d --name package-down -p 3000:3000 --restart unless-stopped -e PRELOAD_REPOS=default package-down
```

发布 Release 后也会同步推送 GHCR 镜像，可直接运行：

```bash
docker run -d --name package-down -p 3000:3000 --restart unless-stopped ghcr.io/senhao-xu/package-down:latest
```

## 发布 Release

推送到 `master` 会自动触发 GitHub Actions，并使用当天日期作为 Release tag，例如 `20260522`。也可以手动推送日期格式 tag 触发发布。

- `package-down-linux-x86_64.tar.gz`
- `package-down-linux-arm64.tar.gz`
- `package-down-windows-x86_64.zip`
- `checksums.txt`

同时会发布 Docker 镜像：

- `ghcr.io/senhao-xu/package-down:<tag>`
- `ghcr.io/senhao-xu/package-down:latest`

自动发布：

```bash
git push origin master
```

手动指定日期 tag 发布：

```bash
git tag 20260522
git push origin 20260522
```

也可以在 GitHub Actions 页面手动运行 `Release` workflow，并填写 tag。

## 快速部署

Linux 会自动识别 `x86_64` / `arm64` 架构，下载最新 Release、安装到 `/opt/package-down` 并启动服务：

```bash
curl -fsSL https://raw.githubusercontent.com/senhao-xu/package-down/main/scripts/install.sh | sudo sh
```

调试时也可以前台启动，直接查看仓库加载进度：

```bash
cd /opt/package-down
./package-down
```

Windows PowerShell 下载并启动：

```powershell
powershell -ExecutionPolicy Bypass -Command "iwr -UseBasicParsing https://raw.githubusercontent.com/senhao-xu/package-down/main/scripts/install.ps1 | iex"
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
PRELOAD_REPOS=none
PRELOAD_BLOCKING=false
PRELOAD_DELAY_MS=2000
PRELOAD_REPO_PAUSE_MS=500
PRELOAD_TIMEOUT_MS=600000
ALLOW_DIRECT_URLS=true
```

`REPO_URLS` 用于自定义 RPM 仓库，必须指向包含 `repodata/repomd.xml` 的 RPM 仓库根路径。自定义仓库 URL 可以使用 `{arch}` 占位符，例如：

```bash
REPO_URLS=https://repo.example.com/os/{arch}/BaseOS/,https://repo.example.com/os/{arch}/AppStream/
```

默认端口是 `3000`，默认目标系统是 `AlmaLinux 9 / x86_64`。

默认不在启动时预加载仓库元数据，服务会先轻量启动，下载请求会先返回 ZIP 流，再按需解析仓库。这样启动占用最低，也不会出现浏览器一直没有响应的感觉。

页面右侧“仓库配置”会展示所有支持系统和架构的源索引状态。服务启动后默认不加载任何索引；需要提前准备某个系统时，可以在该区域点击对应条目的“加载索引”，页面会显示加载进度。

如需提前预热仓库，可打开后台预热；页面右侧“仓库预热”会显示当前进度，日志里也会输出每个仓库的加载进度：

- `PRELOAD_REPOS=none`：默认值，不在启动时预加载，首次下载时按需加载。
- `PRELOAD_REPOS=default`：后台预热默认系统和默认架构；服务会立即启动。
- `PRELOAD_REPOS=all`：后台预热所有内置系统和架构，占用内存更多，不建议小机器使用。
- `PRELOAD_BLOCKING=true`：恢复旧行为，仓库预热完成后才启动 Web 服务。
- `PRELOAD_DELAY_MS=2000`：后台预热延迟启动时间，默认 2 秒。
- `PRELOAD_REPO_PAUSE_MS=500`：每个仓库预热之间的暂停时间，降低启动后资源尖峰。
- `PRELOAD_TIMEOUT_MS=600000`：预加载总超时时间，默认 10 分钟。

## 依赖下载说明

- RPM：使用 primary metadata 内的 `requires` / `provides` / 常见 file provides 做递归解析。
- Ubuntu：使用 `Depends` 和 `Pre-Depends` 做递归解析，版本约束会被忽略，优先选择仓库内可用的较新版本。
- 直接 URL 下载无法解析依赖，只会下载该 URL 指向的文件。
- 为避免依赖链过大，默认最多解析 `300` 个包，可通过 `MAX_RESOLVED_PACKAGES` 调整。
- 未预加载的系统仓库第一次使用时仍需要下载仓库元数据；后续会按 `CACHE_TTL_MS` 缓存。
