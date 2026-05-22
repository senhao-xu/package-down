# AI Context

本文档给后续 AI 或维护者快速理解项目状态、近期改动和实现逻辑使用。面向维护，不替代用户 README。

## 项目概览

Package Proxy 是一个 Go 标准库实现的单二进制 Web 服务。用户输入系统包名或 `.rpm` / `.deb` URL，服务端按目标系统仓库解析包和依赖，然后把远端包文件流式写入 ZIP 响应。

核心文件：

- `main.go`：全部服务端逻辑、仓库 profile、缓存、依赖解析、HTTP handler。
- `main_test.go`：当前基础测试，覆盖直接 URL 下载和无结果 manifest。
- `static/index.html`：前端页面和少量内联 JS。
- `static/styles.css`：前端样式。
- `Dockerfile`：Docker 镜像构建。
- `scripts/install.sh`：Linux 一行安装脚本。
- `scripts/install.ps1`：Windows PowerShell 一行安装脚本。
- `.github/workflows/release.yml`：Release 二进制和 GHCR Docker 镜像发布。

## 近期修改

### 修复预热编译问题

`main.go` 中已有预热状态机函数，但缺少部分结构和入口，导致 `go run .` / `go test ./...` 编译失败。

已补齐：

- `appConfig` 中的预热配置字段：
  - `PreloadMode`
  - `PreloadBlocking`
  - `PreloadDelay`
  - `PreloadRepoPause`
  - `PreloadTimeout`
- `preloadSnapshot` 结构。
- 全局 `preloadMutex` / `preloadState`。
- `startRepositoryPreload` 启动入口。
- `/api/preload/start` 的 `handlePreloadStart` handler。

保留语义：

- 默认 `PRELOAD_REPOS=none`，服务启动时不预热，下载时按需加载仓库元数据。
- `PRELOAD_REPOS=default` 预热默认 profile 和默认架构。
- `PRELOAD_REPOS=all` 预热所有内置 profile 和架构。
- `PRELOAD_BLOCKING=true` 时先预热再启动 HTTP 服务。
- 非阻塞预热会按 `PRELOAD_DELAY_MS` 延迟后台启动。

### 增加按系统索引加载

仓库配置面板已改为展示所有支持系统和架构的源索引状态。启动时默认不加载任何索引；用户可在页面右侧对某个系统/架构点击“加载索引”，后端只加载该 profile + arch 对应的仓库元数据。

相关接口：

- `GET /api/indexes`：返回所有 profile + arch 的索引状态。
- `POST /api/preload/start`：接收 `osProfile` 和 `arch`，触发对应索引加载。
- `GET /api/config`：同时返回 `indexes`，供页面初始化渲染。

索引加载使用 `ensureProfileRepos`，进度写入 `loadStates`，前端轮询 `/api/indexes` 刷新每个系统索引的状态。

### 增加 Docker 支持

新增 `Dockerfile`，使用多阶段构建：

1. `golang:1.22-alpine` 编译静态二进制。
2. `alpine:3.20` 运行，安装 `ca-certificates`，暴露 `3000`。

新增 `.dockerignore`，排除 git、构建产物、日志、包文件和本地环境文件。

### 保留二进制发布并增加镜像发布

`.github/workflows/release.yml` 仍保留原二进制 Release 资产：

- `package-down-linux-x86_64.tar.gz`
- `package-down-linux-arm64.tar.gz`
- `package-down-windows-x86_64.zip`
- `checksums.txt`

同时新增 GHCR 镜像发布：

- `ghcr.io/senhao-xu/package-down:<tag>`
- `ghcr.io/senhao-xu/package-down:latest`

Release tag 规则已从 `v*` 改为数字开头 tag，例如 `2026521`。推送 `2026521` 会触发发布。

### 增加一行安装脚本

新增：

- `scripts/install.sh`
- `scripts/install.ps1`

README 中的一行安装命令指向 GitHub raw 文件：

```bash
curl -fsSL https://raw.githubusercontent.com/senhao-xu/package-down/main/scripts/install.sh | sudo sh
```

```powershell
powershell -ExecutionPolicy Bypass -Command "iwr -UseBasicParsing https://raw.githubusercontent.com/senhao-xu/package-down/main/scripts/install.ps1 | iex"
```

Linux 脚本会自动识别 `x86_64/amd64` 和 `aarch64/arm64`，下载 latest Release 对应 tarball。若存在 `systemctl`，会写入 `/etc/systemd/system/package-down.service` 并启用服务；否则使用 `nohup` 后台启动。

Windows 脚本下载 latest Release zip，解压到 `C:\package-down`，然后隐藏窗口启动 `package-down.exe`。

### 忽略本地日志

`.gitignore` 新增 `*.log`，避免本地启动日志进入待提交列表。

## 下载源加载逻辑

下载源分为内置 profile 和环境变量自定义源。

### 内置 profile

定义在 `main.go` 的 `profiles` map 中。每个 profile 包含：

- `ID`
- `Label`
- `Family`
- `Version`
- `PackageType`
- `DefaultArch`
- `Arches`
- `Repos`

当前包含：

- Ubuntu 20.04 / 22.04 / 24.04
- CentOS 7.9
- CentOS Stream 9
- AlmaLinux 8 / 9
- Rocky Linux 9

RPM profile 使用仓库根路径，服务会读取 `repodata/repomd.xml` 并定位 primary metadata。

Ubuntu DEB profile 使用 `Packages.gz` 完整路径。Ubuntu 使用 repo `Tags` 区分 `amd64` 和 `arm64` 镜像：

- `archive.ubuntu.com` 用于 `amd64`
- `ports.ubuntu.com` 用于 `arm64`

### 自定义源

`loadConfig` 读取 `REPO_URLS`。如果存在，会注入 `custom` profile，包类型固定为 RPM。

规则：

- `REPO_URLS` 使用逗号分隔。
- URL 可以包含 `{arch}` 占位符。
- 如果没有设置 `DEFAULT_OS_PROFILE`，默认 profile 自动切换为 `custom`。
- `REPO_ARCH` 控制 custom profile 默认架构。

### 请求时选源

`handleDownload` 调用 `resolveDownloadContext`：

1. 从表单读取 `osProfile` 和 `arch`。
2. 未指定时使用 `config.DefaultProfileID` 和 profile 默认架构。
3. 调用 `buildRepoURLs` 展开 repo 模板。
4. 生成 `downloadContext`，包含 profile、架构、支持架构集合、repo URL 和客户端信息。

`buildRepoURLs` 会替换：

- `{arch}`
- `{centos7arch}`

并调用 `ensureRepoURL` 做仓库 URL 规范化。

## 依赖解析逻辑

`resolveRequestedFiles` 会把输入拆成两类：

- 直接 `.rpm` / `.deb` URL
- 包名

直接 URL：

- 走 `directRemoteFile`。
- 不解析依赖。
- 受 `ALLOW_DIRECT_URLS` 控制。

包名：

- 调用 `resolvePackageClosure`。
- 先通过 `loadCombinedIndex` 加载目标 profile 的所有仓库索引。
- 用 `findBestPackage` 按名称和 providers 查找最佳候选包。
- 如果 `includeDeps=true`，递归解析依赖。
- `includeDeps` 由用户在页面选择，默认值为 `true`。

RPM：

- 解析 primary metadata 中的 package、location、version、format、requires、provides。
- 依赖来自 `Requires`。
- providers 也会纳入查找。
- 忽略 `rpmlib(...)`、`config(...)`、`module(...)` 等不应下载的依赖。

DEB：

- 解析 `Packages.gz`。
- 依赖来自 `Depends` 和 `Pre-Depends`。
- `Provides` 也会纳入查找。
- 版本约束会被规范化时移除。
- 多个 alternatives 时选第一个可规范化名称。

依赖数量由 `MAX_RESOLVED_PACKAGES` 限制，默认 300。

## 缓存和并发加载

仓库索引缓存：

- `repoCache` 按 `kind|url` 缓存。
- TTL 来自 `CACHE_TTL_MS`，默认 30 分钟。

并发加载合并：

- `repoLoads` 用于合并相同仓库的并发加载。
- 第二个请求会等待第一个请求的 `Done` channel。

profile 级加载状态：

- `loadStates` 记录 profile + arch 级别加载状态。
- `loadSignals` 合并相同 profile + arch 的加载任务。
- `/api/config` 和 `/api/preload` 可用于前端展示状态。

## ZIP 流式下载逻辑

`handleDownload` 不会先把包下载到磁盘。

流程：

1. 设置响应头 `Content-Type: application/zip`。
2. 创建 `zip.Writer` 直接写 `http.ResponseWriter`。
3. 先写入 `README.txt`，让浏览器尽快开始接收 ZIP 流。
4. 解析包和依赖。
5. 对每个 `remoteFile` 调用 `appendRemotePackage`。
6. `appendRemotePackage` 通过 HTTP GET 远端包文件，并用 `io.Copy` 写入 ZIP entry。
7. 最后写入 `manifest.json`。

注意：不要改成先把所有包读入内存或落盘。该项目的核心特性是流式转发和流式压缩。

## 配置项

主要环境变量：

- `PORT`
- `REPO_URLS`
- `DEFAULT_OS_PROFILE`
- `REPO_ARCH`
- `MAX_PACKAGES`
- `MAX_RESOLVED_PACKAGES`
- `CACHE_TTL_MS`
- `REQUEST_TIMEOUT_MS`
- `PRELOAD_REPOS`
- `PRELOAD_BLOCKING`
- `PRELOAD_DELAY_MS`
- `PRELOAD_REPO_PAUSE_MS`
- `PRELOAD_TIMEOUT_MS`
- `ALLOW_DIRECT_URLS`

配置解析集中在 `loadConfig` 和底部 `env*` helper。

## 发布逻辑

Release workflow 触发：

- 推送数字开头 tag，例如 `2026521`。
- 或手动运行 `Release` workflow 并输入 tag。

发布步骤：

1. checkout。
2. setup Go。
3. 解析 tag。
4. `go test ./...`。
5. 构建三份二进制资产。
6. 生成 checksums。
7. 使用 Docker buildx 发布多架构 GHCR 镜像。
8. 创建或更新 GitHub Release。

## 验证记录

已执行并通过：

```bash
go test ./...
```

PowerShell 安装脚本通过本地语法检查。

本地未执行 Docker build，因为当前机器没有 Docker。Docker 镜像构建交给 GitHub Actions。

## 维护注意事项

- 保持标准库 only，不引入第三方依赖，除非非常必要。
- 服务端逻辑继续放在 `main.go`，该项目目前就是单文件 Go 服务。
- 用户可见中文文案应保持简体中文。
- 下载包体必须继续使用 `io.Copy` 从远端响应流写入 ZIP entry。
- 添加新系统源时，优先新增 profile；Ubuntu 这类分架构镜像需使用 `Tags` 过滤。
- 修改预热逻辑时，应通过 `updatePreloadStatus` 更新状态，保持 UI 一致。
- 修改 release 逻辑时，必须保留二进制资产发布；Docker 是新增发布渠道，不替代二进制。
