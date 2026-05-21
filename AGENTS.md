# AGENTS.md

This file provides guidance to Codex (Codex.ai/code) when working with code in this repository.

## Commands

- Run locally: `go run .` (serves at http://localhost:3000)
- Build (Windows): `go build -o package-down.exe .`
- Build (Linux): `go build -o package-down .`
- Release-style build (matches CI): `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o package-down .`
- Run all tests: `go test ./...`
- Run a single test: `go test -run TestDirectPackageDownloadWritesZipFile`
- Trigger a release: push a `v*` tag (or run the `Release` workflow manually) — CI builds linux x86_64/arm64 + windows x86_64 and publishes to GitHub Releases.

## Architecture

Single-binary Go service (Go 1.22, standard library only). All code lives in `main.go` (~1900 lines) plus `main_test.go`. Frontend assets in `static/index.html` and `static/styles.css` are compiled in via `//go:embed static/*` so the deployed artifact is one executable.

### Request flow (`/download`)

1. `handleDownload` parses the form, builds a `downloadContext` (profile + arch + repo URLs + client info) via `resolveDownloadContext`.
2. `resolveRequestedFiles` separates direct `.rpm`/`.deb` URLs from package names. Names go through `resolvePackageClosure`, which walks `requires`/`provides` (RPM) or `Depends`/`Pre-Depends` (DEB) recursively against the loaded repo index, capped at `MAX_RESOLVED_PACKAGES`.
3. The handler opens a `zip.Writer` directly on the `http.ResponseWriter` and streams each remote package via `appendRemotePackage` — bytes are copied from the upstream HTTP response straight into the zip entry, never buffered to disk.
4. `manifest.json` (sources, sizes, errors) and, on total failure, a `README.txt` are written into the zip before close.

### Repository indexing

- Profiles (AlmaLinux 8/9, CentOS 7.9, Rocky 9, CentOS Stream 9, Ubuntu 22.04/24.04) are declared as a `map[string]profile` near the top of `main.go`. Each profile has `repoTemplate`s with `{arch}` (and `{centos7arch}` for CentOS 7) placeholders expanded by `buildRepoURLs`.
- Setting `REPO_URLS` injects a synthetic `custom` RPM profile and (unless `DEFAULT_OS_PROFILE` is set) makes it the default.
- `loadRepoIndex` is the cache layer: keyed by `kind|url`, results stored in `repoCache` with `CacheTTL`, and concurrent loads of the same key are coalesced through `repoLoads` (`repoLoadCall` with a `chan struct{}`). RPM repos are fetched via `repodata/repomd.xml` → primary XML; DEB repos via `Packages.gz`.
- Ubuntu repo templates use `Tags` (`amd64` vs `arm64`) so `buildRepoURLs` only enables the right mirrors per target architecture (archive.ubuntu.com vs ports.ubuntu.com).

### Preload state machine

`startRepositoryPreload` runs based on `PRELOAD_REPOS` (`none`/`default`/`all`) and `PRELOAD_BLOCKING`. Progress is written to a single `preloadState preloadSnapshot` guarded by `preloadMutex` and surfaced via `/api/preload`, which the frontend polls. When editing preload logic, always update progress through `updatePreloadStatus` to keep the UI consistent.

### Configuration

All knobs are env-vars parsed in `loadConfig` into `appConfig` (cache TTL, package/resolved limits, request timeout, preload mode/blocking/delay/pause/timeout, `ALLOW_DIRECT_URLS`). The full list and defaults are documented in `README.md`.

### Endpoints

- `GET /` — embedded `static/index.html` (or other static files).
- `GET /api/config` — profiles, default profile, current arch, preload snapshot. Drives the UI.
- `GET /api/os-detect` — User-Agent based OS/arch guess for the form defaults.
- `GET /api/preload` — current `preloadSnapshot` for polling.
- `POST /download` — form submit, returns the streaming zip.

## Conventions

- Stdlib only — do not introduce third-party deps without a strong reason; the single-binary, zero-dep deploy story is core to the project.
- Keep all server logic in `main.go`. New types/helpers go alongside existing ones; the file is intentionally flat.
- User-visible strings (logs, manifest messages, UI copy) are in Simplified Chinese — match that style when editing them.
- The zip is streamed; never accumulate full package bodies in memory. Use `io.Copy` from the upstream response to the zip entry, like `appendRemotePackage`.
