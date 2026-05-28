package main

import (
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"embed"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed static/*
var embeddedFiles embed.FS

type repoKind string

const (
	repoKindRPM repoKind = "rpm"
	repoKindDEB repoKind = "deb"
)

type repoTemplate struct {
	Name string   `json:"name"`
	URL  string   `json:"url"`
	Tags []string `json:"tags,omitempty"`
}

type profile struct {
	ID          string         `json:"id"`
	Label       string         `json:"label"`
	Family      string         `json:"family"`
	Version     string         `json:"version"`
	PackageType repoKind       `json:"packageType"`
	DefaultArch string         `json:"defaultArch"`
	Arches      []string       `json:"arches"`
	Repos       []repoTemplate `json:"-"`
}

type appConfig struct {
	DefaultProfileID string
	CacheTTL         time.Duration
	MaxPackages      int
	MaxResolved      int
	RequestTimeout   time.Duration
	IndexTimeout     time.Duration
	PreloadMode      string
	PreloadBlocking  bool
	PreloadDelay     time.Duration
	PreloadRepoPause time.Duration
	PreloadTimeout   time.Duration
	AllowDirectURL   bool
}

type downloadContext struct {
	Profile  profile
	Arch     string
	Arches   map[string]bool
	RepoURLs []repoInfo
	Client   clientInfo
}

type repoInfo struct {
	Name string   `json:"name"`
	URL  string   `json:"url"`
	Tags []string `json:"tags,omitempty"`
}

type clientInfo struct {
	BrowserReported reportedClient `json:"browserReported"`
	ServerDetected  detectedClient `json:"serverDetected"`
}

type reportedClient struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

type detectedClient struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	UserAgent string `json:"userAgent"`
}

type packageMeta struct {
	Name        string   `json:"name"`
	PackageType repoKind `json:"packageType"`
	Arch        string   `json:"arch"`
	Epoch       string   `json:"epoch,omitempty"`
	Version     string   `json:"version"`
	Release     string   `json:"release,omitempty"`
	Filename    string   `json:"filename"`
	Repo        string   `json:"repo"`
	Source      string   `json:"source"`
	Provides    []string `json:"provides,omitempty"`
	Requires    []string `json:"requires,omitempty"`
	Depends     []string `json:"depends,omitempty"`
}

type packageIndex struct {
	ByName    map[string][]packageMeta
	Providers map[string][]packageMeta
}

type manifest struct {
	StartedAt       string         `json:"startedAt"`
	Requested       []string       `json:"requested"`
	IncludeDeps     bool           `json:"includeDependencies"`
	ResolvedCount   int            `json:"resolvedCount"`
	DependencyLimit int            `json:"dependencyLimit"`
	Target          manifestTarget `json:"target"`
	Client          clientInfo     `json:"client"`
	Files           []manifestFile `json:"files"`
	Errors          []manifestErr  `json:"errors"`
}

type manifestTarget struct {
	OSProfile   string     `json:"osProfile"`
	OSLabel     string     `json:"osLabel"`
	PackageType repoKind   `json:"packageType"`
	Arch        string     `json:"arch"`
	Repos       []repoInfo `json:"repos"`
}

type manifestFile struct {
	Requested string       `json:"requested"`
	Name      string       `json:"name"`
	Source    string       `json:"source"`
	Repo      string       `json:"repo,omitempty"`
	Size      *int64       `json:"size"`
	Package   *packageMeta `json:"package,omitempty"`
}

type manifestErr struct {
	Requested string `json:"requested,omitempty"`
	Package   string `json:"package,omitempty"`
	Repo      string `json:"repo,omitempty"`
	Source    string `json:"source,omitempty"`
	Message   string `json:"message"`
}

type resolvedPackage struct {
	Packages   []packageMeta
	RepoErrors []manifestErr
}

type remoteFile struct {
	Requested string
	URL       string
	Filename  string
	Repo      string
	Package   *packageMeta
}

type repoCacheEntry struct {
	LoadedAt time.Time
	Index    packageIndex
}

type repoLoadCall struct {
	Done  chan struct{}
	Index packageIndex
	Err   error
}

type repoLoadState struct {
	Status      string `json:"status"`
	Message     string `json:"message"`
	ReposTotal  int    `json:"reposTotal"`
	ReposDone   int    `json:"reposDone"`
	Packages    int    `json:"packages"`
	CurrentRepo string `json:"currentRepo,omitempty"`
	StartedAt   string `json:"startedAt,omitempty"`
	FinishedAt  string `json:"finishedAt,omitempty"`
	LastError   string `json:"lastError,omitempty"`
	OSProfile   string `json:"osProfile,omitempty"`
	OSLabel     string `json:"osLabel,omitempty"`
	Arch        string `json:"arch,omitempty"`
}

type profileIndexState struct {
	OSProfile   string     `json:"osProfile"`
	OSLabel     string     `json:"osLabel"`
	PackageType repoKind   `json:"packageType"`
	Arch        string     `json:"arch"`
	Repos       []repoInfo `json:"repos"`
	Status      string     `json:"status"`
	Message     string     `json:"message"`
	ReposTotal  int        `json:"reposTotal"`
	ReposDone   int        `json:"reposDone"`
	Packages    int        `json:"packages"`
	CurrentRepo string     `json:"currentRepo,omitempty"`
	StartedAt   string     `json:"startedAt,omitempty"`
	FinishedAt  string     `json:"finishedAt,omitempty"`
	LastError   string     `json:"lastError,omitempty"`
}

type preloadSnapshot struct {
	Mode          string `json:"mode"`
	Blocking      bool   `json:"blocking"`
	Status        string `json:"status"`
	Message       string `json:"message"`
	TargetsTotal  int    `json:"targetsTotal"`
	TargetsDone   int    `json:"targetsDone"`
	ReposTotal    int    `json:"reposTotal"`
	ReposDone     int    `json:"reposDone"`
	Packages      int    `json:"packages"`
	CurrentTarget string `json:"currentTarget,omitempty"`
	CurrentRepo   string `json:"currentRepo,omitempty"`
	StartedAt     string `json:"startedAt,omitempty"`
	FinishedAt    string `json:"finishedAt,omitempty"`
	LastError     string `json:"lastError,omitempty"`
}

var (
	profiles = map[string]profile{
		"almalinux-9": {
			ID:          "almalinux-9",
			Label:       "AlmaLinux 9",
			Family:      "RHEL",
			Version:     "9",
			PackageType: repoKindRPM,
			DefaultArch: "x86_64",
			Arches:      []string{"x86_64", "aarch64"},
			Repos: []repoTemplate{
				{Name: "BaseOS", URL: "https://repo.almalinux.org/almalinux/9/BaseOS/{arch}/os/"},
				{Name: "AppStream", URL: "https://repo.almalinux.org/almalinux/9/AppStream/{arch}/os/"},
				{Name: "CRB", URL: "https://repo.almalinux.org/almalinux/9/CRB/{arch}/os/"},
			},
		},
		"almalinux-8": {
			ID:          "almalinux-8",
			Label:       "AlmaLinux 8",
			Family:      "RHEL",
			Version:     "8",
			PackageType: repoKindRPM,
			DefaultArch: "x86_64",
			Arches:      []string{"x86_64", "aarch64"},
			Repos: []repoTemplate{
				{Name: "BaseOS", URL: "https://repo.almalinux.org/almalinux/8/BaseOS/{arch}/os/"},
				{Name: "AppStream", URL: "https://repo.almalinux.org/almalinux/8/AppStream/{arch}/os/"},
				{Name: "PowerTools", URL: "https://repo.almalinux.org/almalinux/8/PowerTools/{arch}/os/"},
			},
		},
		"centos-7": {
			ID:          "centos-7",
			Label:       "CentOS 7.9",
			Family:      "RHEL",
			Version:     "7.9.2009",
			PackageType: repoKindRPM,
			DefaultArch: "x86_64",
			Arches:      []string{"x86_64", "aarch64"},
			Repos: []repoTemplate{
				{Name: "Base", URL: "https://vault.centos.org/{centos7arch}/os/{arch}/"},
				{Name: "Updates", URL: "https://vault.centos.org/{centos7arch}/updates/{arch}/"},
				{Name: "Extras", URL: "https://vault.centos.org/{centos7arch}/extras/{arch}/"},
			},
		},
		"rocky-9": {
			ID:          "rocky-9",
			Label:       "Rocky Linux 9",
			Family:      "RHEL",
			Version:     "9",
			PackageType: repoKindRPM,
			DefaultArch: "x86_64",
			Arches:      []string{"x86_64", "aarch64"},
			Repos: []repoTemplate{
				{Name: "BaseOS", URL: "https://download.rockylinux.org/pub/rocky/9/BaseOS/{arch}/os/"},
				{Name: "AppStream", URL: "https://download.rockylinux.org/pub/rocky/9/AppStream/{arch}/os/"},
				{Name: "CRB", URL: "https://download.rockylinux.org/pub/rocky/9/CRB/{arch}/os/"},
			},
		},
		"centos-stream-9": {
			ID:          "centos-stream-9",
			Label:       "CentOS Stream 9",
			Family:      "RHEL",
			Version:     "9-stream",
			PackageType: repoKindRPM,
			DefaultArch: "x86_64",
			Arches:      []string{"x86_64", "aarch64"},
			Repos: []repoTemplate{
				{Name: "BaseOS", URL: "https://mirror.stream.centos.org/9-stream/BaseOS/{arch}/os/"},
				{Name: "AppStream", URL: "https://mirror.stream.centos.org/9-stream/AppStream/{arch}/os/"},
				{Name: "CRB", URL: "https://mirror.stream.centos.org/9-stream/CRB/{arch}/os/"},
			},
		},
		"kylin-v10-sp3-2403": {
			ID:          "kylin-v10-sp3-2403",
			Label:       "银河麒麟 V10 SP3 2403",
			Family:      "Kylin",
			Version:     "V10 SP3 2403",
			PackageType: repoKindRPM,
			DefaultArch: "x86_64",
			Arches:      []string{"x86_64", "aarch64"},
			Repos: []repoTemplate{
				{Name: "Base", URL: "https://update.cs2c.com.cn/NS/V10/V10SP3-2403/os/adv/lic/base/{arch}/"},
				{Name: "Updates", URL: "https://update.cs2c.com.cn/NS/V10/V10SP3-2403/os/adv/lic/updates/{arch}/"},
			},
		},
		"ubuntu-24.04": {
			ID:          "ubuntu-24.04",
			Label:       "Ubuntu 24.04 LTS",
			Family:      "Ubuntu",
			Version:     "24.04",
			PackageType: repoKindDEB,
			DefaultArch: "amd64",
			Arches:      []string{"amd64", "arm64"},
			Repos: []repoTemplate{
				{Name: "main", URL: "https://archive.ubuntu.com/ubuntu/dists/noble/main/binary-{arch}/Packages.gz", Tags: []string{"amd64"}},
				{Name: "universe", URL: "https://archive.ubuntu.com/ubuntu/dists/noble/universe/binary-{arch}/Packages.gz", Tags: []string{"amd64"}},
				{Name: "updates-main", URL: "https://archive.ubuntu.com/ubuntu/dists/noble-updates/main/binary-{arch}/Packages.gz", Tags: []string{"amd64"}},
				{Name: "updates-universe", URL: "https://archive.ubuntu.com/ubuntu/dists/noble-updates/universe/binary-{arch}/Packages.gz", Tags: []string{"amd64"}},
				{Name: "security-main", URL: "https://security.ubuntu.com/ubuntu/dists/noble-security/main/binary-{arch}/Packages.gz", Tags: []string{"amd64"}},
				{Name: "security-universe", URL: "https://security.ubuntu.com/ubuntu/dists/noble-security/universe/binary-{arch}/Packages.gz", Tags: []string{"amd64"}},
				{Name: "ports-main", URL: "https://ports.ubuntu.com/ubuntu-ports/dists/noble/main/binary-{arch}/Packages.gz", Tags: []string{"arm64"}},
				{Name: "ports-universe", URL: "https://ports.ubuntu.com/ubuntu-ports/dists/noble/universe/binary-{arch}/Packages.gz", Tags: []string{"arm64"}},
				{Name: "ports-updates-main", URL: "https://ports.ubuntu.com/ubuntu-ports/dists/noble-updates/main/binary-{arch}/Packages.gz", Tags: []string{"arm64"}},
				{Name: "ports-updates-universe", URL: "https://ports.ubuntu.com/ubuntu-ports/dists/noble-updates/universe/binary-{arch}/Packages.gz", Tags: []string{"arm64"}},
				{Name: "ports-security-main", URL: "https://ports.ubuntu.com/ubuntu-ports/dists/noble-security/main/binary-{arch}/Packages.gz", Tags: []string{"arm64"}},
				{Name: "ports-security-universe", URL: "https://ports.ubuntu.com/ubuntu-ports/dists/noble-security/universe/binary-{arch}/Packages.gz", Tags: []string{"arm64"}},
			},
		},
		"ubuntu-22.04": {
			ID:          "ubuntu-22.04",
			Label:       "Ubuntu 22.04 LTS",
			Family:      "Ubuntu",
			Version:     "22.04",
			PackageType: repoKindDEB,
			DefaultArch: "amd64",
			Arches:      []string{"amd64", "arm64"},
			Repos: []repoTemplate{
				{Name: "main", URL: "https://archive.ubuntu.com/ubuntu/dists/jammy/main/binary-{arch}/Packages.gz", Tags: []string{"amd64"}},
				{Name: "universe", URL: "https://archive.ubuntu.com/ubuntu/dists/jammy/universe/binary-{arch}/Packages.gz", Tags: []string{"amd64"}},
				{Name: "updates-main", URL: "https://archive.ubuntu.com/ubuntu/dists/jammy-updates/main/binary-{arch}/Packages.gz", Tags: []string{"amd64"}},
				{Name: "updates-universe", URL: "https://archive.ubuntu.com/ubuntu/dists/jammy-updates/universe/binary-{arch}/Packages.gz", Tags: []string{"amd64"}},
				{Name: "security-main", URL: "https://security.ubuntu.com/ubuntu/dists/jammy-security/main/binary-{arch}/Packages.gz", Tags: []string{"amd64"}},
				{Name: "security-universe", URL: "https://security.ubuntu.com/ubuntu/dists/jammy-security/universe/binary-{arch}/Packages.gz", Tags: []string{"amd64"}},
				{Name: "ports-main", URL: "https://ports.ubuntu.com/ubuntu-ports/dists/jammy/main/binary-{arch}/Packages.gz", Tags: []string{"arm64"}},
				{Name: "ports-universe", URL: "https://ports.ubuntu.com/ubuntu-ports/dists/jammy/universe/binary-{arch}/Packages.gz", Tags: []string{"arm64"}},
				{Name: "ports-updates-main", URL: "https://ports.ubuntu.com/ubuntu-ports/dists/jammy-updates/main/binary-{arch}/Packages.gz", Tags: []string{"arm64"}},
				{Name: "ports-updates-universe", URL: "https://ports.ubuntu.com/ubuntu-ports/dists/jammy-updates/universe/binary-{arch}/Packages.gz", Tags: []string{"arm64"}},
				{Name: "ports-security-main", URL: "https://ports.ubuntu.com/ubuntu-ports/dists/jammy-security/main/binary-{arch}/Packages.gz", Tags: []string{"arm64"}},
				{Name: "ports-security-universe", URL: "https://ports.ubuntu.com/ubuntu-ports/dists/jammy-security/universe/binary-{arch}/Packages.gz", Tags: []string{"arm64"}},
			},
		},
		"ubuntu-20.04": {
			ID:          "ubuntu-20.04",
			Label:       "Ubuntu 20.04 LTS",
			Family:      "Ubuntu",
			Version:     "20.04",
			PackageType: repoKindDEB,
			DefaultArch: "amd64",
			Arches:      []string{"amd64", "arm64"},
			Repos: []repoTemplate{
				{Name: "main", URL: "https://archive.ubuntu.com/ubuntu/dists/focal/main/binary-{arch}/Packages.gz", Tags: []string{"amd64"}},
				{Name: "universe", URL: "https://archive.ubuntu.com/ubuntu/dists/focal/universe/binary-{arch}/Packages.gz", Tags: []string{"amd64"}},
				{Name: "updates-main", URL: "https://archive.ubuntu.com/ubuntu/dists/focal-updates/main/binary-{arch}/Packages.gz", Tags: []string{"amd64"}},
				{Name: "updates-universe", URL: "https://archive.ubuntu.com/ubuntu/dists/focal-updates/universe/binary-{arch}/Packages.gz", Tags: []string{"amd64"}},
				{Name: "security-main", URL: "https://security.ubuntu.com/ubuntu/dists/focal-security/main/binary-{arch}/Packages.gz", Tags: []string{"amd64"}},
				{Name: "security-universe", URL: "https://security.ubuntu.com/ubuntu/dists/focal-security/universe/binary-{arch}/Packages.gz", Tags: []string{"amd64"}},
				{Name: "ports-main", URL: "https://ports.ubuntu.com/ubuntu-ports/dists/focal/main/binary-{arch}/Packages.gz", Tags: []string{"arm64"}},
				{Name: "ports-universe", URL: "https://ports.ubuntu.com/ubuntu-ports/dists/focal/universe/binary-{arch}/Packages.gz", Tags: []string{"arm64"}},
				{Name: "ports-updates-main", URL: "https://ports.ubuntu.com/ubuntu-ports/dists/focal-updates/main/binary-{arch}/Packages.gz", Tags: []string{"arm64"}},
				{Name: "ports-updates-universe", URL: "https://ports.ubuntu.com/ubuntu-ports/dists/focal-updates/universe/binary-{arch}/Packages.gz", Tags: []string{"arm64"}},
				{Name: "ports-security-main", URL: "https://ports.ubuntu.com/ubuntu-ports/dists/focal-security/main/binary-{arch}/Packages.gz", Tags: []string{"arm64"}},
				{Name: "ports-security-universe", URL: "https://ports.ubuntu.com/ubuntu-ports/dists/focal-security/universe/binary-{arch}/Packages.gz", Tags: []string{"arm64"}},
			},
		},
	}
	config       appConfig
	repoCache    = map[string]repoCacheEntry{}
	repoLoads    = map[string]*repoLoadCall{}
	cacheMutex   sync.Mutex
	loadMutex    sync.Mutex
	loadStates   = map[string]*repoLoadState{}
	loadSignals  = map[string]chan struct{}{}
	preloadMutex sync.Mutex
	preloadState = preloadSnapshot{
		Mode:    "none",
		Status:  "disabled",
		Message: "仓库预热已关闭",
	}
	httpClient   = &http.Client{}
	tokenRe      = regexp.MustCompile(`[A-Za-z]+|\d+`)
	debDepSplit  = regexp.MustCompile(`\s*,\s*`)
	debAltSplit  = regexp.MustCompile(`\s*\|\s*`)
	rpmDepNameRe = regexp.MustCompile(`^[A-Za-z0-9_+./()@:-]+`)
)

func main() {
	config = loadConfig()

	staticFS, err := fs.Sub(embeddedFiles, "static")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", serveIndexOrStatic(staticFS))
	mux.HandleFunc("/api/config", handleConfig)
	mux.HandleFunc("/api/indexes", handleIndexStatus)
	mux.HandleFunc("/api/os-detect", handleOSDetect)
	mux.HandleFunc("/api/preload", handlePreloadStatus)
	mux.HandleFunc("/api/preload/start", handlePreloadStart)
	mux.HandleFunc("/download", handleDownload)

	startRepositoryPreload(context.Background())

	addr := ":" + getenv("PORT", "3000")

	log.Printf("package-down is running at http://localhost%s", addr)
	log.Printf("default target: %s", profiles[config.DefaultProfileID].Label)

	if err := http.ListenAndServe(addr, logRequest(mux)); err != nil {
		log.Fatal(err)
	}
}

func loadConfig() appConfig {
	customRepos := splitList(os.Getenv("REPO_URLS"))
	defaultProfileID := getenv("DEFAULT_OS_PROFILE", "almalinux-9")

	if len(customRepos) > 0 {
		profiles["custom"] = profile{
			ID:          "custom",
			Label:       "自定义 RPM 仓库",
			Family:      "Custom",
			Version:     "custom",
			PackageType: repoKindRPM,
			DefaultArch: normalizeTargetArchOrDefault(getenv("REPO_ARCH", "x86_64"), "x86_64"),
			Arches:      []string{"x86_64", "aarch64"},
			Repos:       repoTemplatesFromURLs(customRepos),
		}

		if os.Getenv("DEFAULT_OS_PROFILE") == "" {
			defaultProfileID = "custom"
		}
	}

	if _, ok := profiles[defaultProfileID]; !ok {
		defaultProfileID = "almalinux-9"
	}

	return appConfig{
		DefaultProfileID: defaultProfileID,
		CacheTTL:         envDuration("CACHE_TTL_MS", 30*time.Minute),
		MaxPackages:      envInt("MAX_PACKAGES", 50),
		MaxResolved:      envInt("MAX_RESOLVED_PACKAGES", 300),
		RequestTimeout:   envDuration("REQUEST_TIMEOUT_MS", 2*time.Minute),
		IndexTimeout:     envDuration("INDEX_TIMEOUT_MS", 10*time.Minute),
		PreloadMode:      normalizePreloadMode(getenv("PRELOAD_REPOS", "none")),
		PreloadBlocking:  envBool("PRELOAD_BLOCKING", false),
		PreloadDelay:     envDurationAllowZero("PRELOAD_DELAY_MS", 2*time.Second),
		PreloadRepoPause: envDurationAllowZero("PRELOAD_REPO_PAUSE_MS", 500*time.Millisecond),
		PreloadTimeout:   envDuration("PRELOAD_TIMEOUT_MS", 10*time.Minute),
		AllowDirectURL:   strings.ToLower(os.Getenv("ALLOW_DIRECT_URLS")) != "false" && strings.ToLower(os.Getenv("ALLOW_DIRECT_RPM_URLS")) != "false",
	}
}

func profileLoadKey(profileID, arch string) string {
	return profileID + "|" + arch
}

func currentLoadStateForKey(key string) repoLoadState {
	loadMutex.Lock()
	defer loadMutex.Unlock()
	if state, ok := loadStates[key]; ok {
		return *state
	}
	return repoLoadState{Status: "idle"}
}

func snapshotAllLoadStates() map[string]repoLoadState {
	loadMutex.Lock()
	defer loadMutex.Unlock()
	out := make(map[string]repoLoadState, len(loadStates))
	for key, state := range loadStates {
		out[key] = *state
	}
	return out
}

func listProfileIndexStates() []profileIndexState {
	items := []profileIndexState{}
	for _, prof := range listProfiles() {
		for _, arch := range prof.Arches {
			values := url.Values{
				"osProfile": {prof.ID},
				"arch":      {arch},
			}
			items = append(items, profileIndexStateForContext(resolveDownloadContext(values, "")))
		}
	}
	return items
}

func profileIndexStateForContext(dctx downloadContext) profileIndexState {
	state := currentLoadStateForKey(profileLoadKey(dctx.Profile.ID, dctx.Arch))
	if state.Status == "" {
		state.Status = "idle"
	}
	if state.Status == "completed" && !allReposCached(dctx) {
		state = repoLoadState{Status: "idle"}
	}
	if state.Status == "idle" && allReposCached(dctx) {
		state = repoLoadState{
			Status:     "completed",
			Message:    "仓库元数据已就绪",
			ReposTotal: len(dctx.RepoURLs),
			ReposDone:  len(dctx.RepoURLs),
			Packages:   cachedPackageCount(dctx),
			OSProfile:  dctx.Profile.ID,
			OSLabel:    dctx.Profile.Label,
			Arch:       dctx.Arch,
			FinishedAt: time.Now().Format(time.RFC3339),
		}
	}
	if state.Message == "" {
		state.Message = "未加载"
	}
	if state.ReposTotal == 0 {
		state.ReposTotal = len(dctx.RepoURLs)
	}

	return profileIndexState{
		OSProfile:   dctx.Profile.ID,
		OSLabel:     dctx.Profile.Label,
		PackageType: dctx.Profile.PackageType,
		Arch:        dctx.Arch,
		Repos:       dctx.RepoURLs,
		Status:      state.Status,
		Message:     state.Message,
		ReposTotal:  state.ReposTotal,
		ReposDone:   state.ReposDone,
		Packages:    state.Packages,
		CurrentRepo: state.CurrentRepo,
		StartedAt:   state.StartedAt,
		FinishedAt:  state.FinishedAt,
		LastError:   state.LastError,
	}
}

// ensureProfileRepos returns a channel that closes once the requested profile's
// repositories are loaded into repoCache. Concurrent callers for the same
// profile|arch share the same load goroutine. If the profile is already
// completed within CacheTTL, the returned channel is already closed.
func ensureProfileRepos(ctx context.Context, dctx downloadContext) <-chan struct{} {
	key := profileLoadKey(dctx.Profile.ID, dctx.Arch)

	loadMutex.Lock()
	if signal, ok := loadSignals[key]; ok {
		loadMutex.Unlock()
		return signal
	}

	if state, ok := loadStates[key]; ok && state.Status == "completed" && allReposCached(dctx) {
		loadMutex.Unlock()
		done := make(chan struct{})
		close(done)
		return done
	}

	if allReposCached(dctx) {
		loadStates[key] = &repoLoadState{
			Status:     "completed",
			Message:    "仓库元数据已就绪",
			ReposTotal: len(dctx.RepoURLs),
			ReposDone:  len(dctx.RepoURLs),
			OSProfile:  dctx.Profile.ID,
			OSLabel:    dctx.Profile.Label,
			Arch:       dctx.Arch,
			FinishedAt: time.Now().Format(time.RFC3339),
		}
		loadMutex.Unlock()
		done := make(chan struct{})
		close(done)
		return done
	}

	signal := make(chan struct{})
	loadSignals[key] = signal
	loadStates[key] = &repoLoadState{
		Status:     "running",
		Message:    "正在加载仓库元数据",
		ReposTotal: len(dctx.RepoURLs),
		OSProfile:  dctx.Profile.ID,
		OSLabel:    dctx.Profile.Label,
		Arch:       dctx.Arch,
		StartedAt:  time.Now().Format(time.RFC3339),
	}
	loadMutex.Unlock()

	go runProfileLoad(ctx, key, dctx, signal)
	return signal
}

func allReposCached(dctx downloadContext) bool {
	if len(dctx.RepoURLs) == 0 {
		return false
	}
	cacheMutex.Lock()
	defer cacheMutex.Unlock()
	for _, repo := range dctx.RepoURLs {
		entry, ok := repoCache[string(dctx.Profile.PackageType)+"|"+repo.URL]
		if !ok || time.Since(entry.LoadedAt) >= config.CacheTTL {
			return false
		}
	}
	return true
}

func cachedPackageCount(dctx downloadContext) int {
	cacheMutex.Lock()
	defer cacheMutex.Unlock()
	total := 0
	for _, repo := range dctx.RepoURLs {
		entry, ok := repoCache[string(dctx.Profile.PackageType)+"|"+repo.URL]
		if !ok || time.Since(entry.LoadedAt) >= config.CacheTTL {
			continue
		}
		total += len(entry.Index.ByName)
	}
	return total
}

func runProfileLoad(ctx context.Context, key string, dctx downloadContext, signal chan struct{}) {
	defer func() {
		loadMutex.Lock()
		delete(loadSignals, key)
		loadMutex.Unlock()
		close(signal)
	}()

	loadCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-loadCtx.Done():
		}
	}()

	total := len(dctx.RepoURLs)
	repoErrors := []string{}

	for index, repo := range dctx.RepoURLs {
		startedAt := time.Now()
		updateLoadState(key, func(state *repoLoadState) {
			state.CurrentRepo = repo.Name
			state.Message = fmt.Sprintf("正在加载仓库 %d/%d：%s", index+1, total, repo.Name)
		})
		log.Printf("load profile %s repo %d/%d start: %s %s", key, index+1, total, repo.Name, repo.URL)

		repoIndex, err := loadRepoIndex(loadCtx, dctx.Profile.PackageType, repo)
		if err != nil {
			repoErrors = append(repoErrors, fmt.Sprintf("%s: %s", repo.Name, err))
			updateLoadState(key, func(state *repoLoadState) {
				state.ReposDone++
				state.Message = fmt.Sprintf("仓库加载失败：%s", repo.Name)
				state.LastError = err.Error()
			})
			log.Printf("load profile %s repo %d/%d failed after %s: %s %s", key, index+1, total, time.Since(startedAt).Round(time.Second), repo.Name, err)
			continue
		}

		updateLoadState(key, func(state *repoLoadState) {
			state.ReposDone++
			state.Packages += len(repoIndex.ByName)
			state.Message = fmt.Sprintf("仓库加载完成：%s，%d 个包", repo.Name, len(repoIndex.ByName))
		})
		log.Printf("load profile %s repo %d/%d done: %s packages=%d duration=%s", key, index+1, total, repo.Name, len(repoIndex.ByName), time.Since(startedAt).Round(time.Second))
	}

	updateLoadState(key, func(state *repoLoadState) {
		state.CurrentRepo = ""
		state.FinishedAt = time.Now().Format(time.RFC3339)
		if state.Packages == 0 && len(repoErrors) > 0 {
			state.Status = "error"
			state.Message = "仓库加载失败"
			state.LastError = strings.Join(repoErrors, "; ")
			return
		}
		state.Status = "completed"
		if len(repoErrors) > 0 {
			state.Message = fmt.Sprintf("加载完成，但有 %d 个仓库失败：%d 个包", len(repoErrors), state.Packages)
			state.LastError = strings.Join(repoErrors, "; ")
		} else {
			state.Message = fmt.Sprintf("加载完成：%d 个包", state.Packages)
			state.LastError = ""
		}
	})
}

func updateLoadState(key string, mutate func(*repoLoadState)) {
	loadMutex.Lock()
	defer loadMutex.Unlock()
	state, ok := loadStates[key]
	if !ok {
		return
	}
	mutate(state)
}

func startRepositoryPreload(ctx context.Context) {
	contexts := preloadDownloadContexts()
	if len(contexts) == 0 {
		updatePreloadStatus(func(state *preloadSnapshot) {
			*state = preloadSnapshot{
				Mode:     config.PreloadMode,
				Blocking: config.PreloadBlocking,
				Status:   "disabled",
				Message:  "仓库预热已关闭",
			}
		})
		return
	}

	if config.PreloadBlocking {
		if err := preloadConfiguredRepositories(ctx, contexts); err != nil {
			log.Printf("repository preload failed: %v", err)
		}
		return
	}

	updatePreloadStatus(func(state *preloadSnapshot) {
		*state = preloadSnapshot{
			Mode:         config.PreloadMode,
			Blocking:     config.PreloadBlocking,
			Status:       "waiting",
			Message:      "等待后台预热启动",
			TargetsTotal: len(contexts),
			ReposTotal:   countPreloadRepos(contexts),
		}
	})

	go func() {
		if err := sleepContext(ctx, config.PreloadDelay); err != nil {
			markPreloadError(err)
			return
		}
		if err := preloadConfiguredRepositories(ctx, contexts); err != nil {
			log.Printf("repository preload failed: %v", err)
		}
	}()
}

func preloadConfiguredRepositories(ctx context.Context, contexts []downloadContext) error {
	preloadCtx, cancel := context.WithTimeout(ctx, config.PreloadTimeout)
	defer cancel()

	updatePreloadStatus(func(state *preloadSnapshot) {
		*state = preloadSnapshot{
			Mode:         config.PreloadMode,
			Blocking:     config.PreloadBlocking,
			Status:       "running",
			Message:      "正在加载仓库元数据",
			TargetsTotal: len(contexts),
			ReposTotal:   countPreloadRepos(contexts),
			StartedAt:    time.Now().Format(time.RFC3339),
		}
	})

	if config.PreloadBlocking {
		log.Printf("preloading repositories before service start: mode=%s targets=%d timeout=%s", config.PreloadMode, len(contexts), config.PreloadTimeout)
	} else {
		log.Printf("preloading repositories in background: mode=%s targets=%d timeout=%s", config.PreloadMode, len(contexts), config.PreloadTimeout)
	}

	for targetIndex, dctx := range contexts {
		startedAt := time.Now()
		targetName := preloadTargetName(dctx)
		updatePreloadStatus(func(state *preloadSnapshot) {
			state.Status = "running"
			state.Message = "正在加载 " + targetName
			state.CurrentTarget = targetName
			state.CurrentRepo = ""
		})
		log.Printf("preload target %d/%d: %s repos=%d", targetIndex+1, len(contexts), targetName, len(dctx.RepoURLs))

		index, errors := loadCombinedIndexWithProgress(preloadCtx, dctx)
		for _, item := range errors {
			log.Printf("preload warning: %s %s %s: %s", dctx.Profile.Label, dctx.Arch, item.Repo, item.Message)
		}

		if len(index.ByName) == 0 {
			err := fmt.Errorf("preload failed: %s has no loaded packages", targetName)
			markPreloadError(err)
			return err
		}

		updatePreloadStatus(func(state *preloadSnapshot) {
			state.TargetsDone = targetIndex + 1
			state.Message = fmt.Sprintf("已加载 %s：%d 个包", targetName, len(index.ByName))
		})
		log.Printf("preloaded %s: %d packages in %s", targetName, len(index.ByName), time.Since(startedAt).Round(time.Second))
	}

	updatePreloadStatus(func(state *preloadSnapshot) {
		state.Status = "completed"
		state.Message = "仓库预热完成"
		state.CurrentTarget = ""
		state.CurrentRepo = ""
		state.FinishedAt = time.Now().Format(time.RFC3339)
	})
	log.Println("repository preload completed")
	return nil
}

func loadCombinedIndexWithProgress(ctx context.Context, dctx downloadContext) (packageIndex, []manifestErr) {
	combined := newPackageIndex()
	errors := []manifestErr{}
	total := len(dctx.RepoURLs)

	for index, repo := range dctx.RepoURLs {
		startedAt := time.Now()
		updatePreloadStatus(func(state *preloadSnapshot) {
			state.CurrentRepo = repo.Name
			state.Message = fmt.Sprintf("正在加载仓库 %d/%d：%s", index+1, total, repo.Name)
		})
		log.Printf("preload repo %d/%d start: %s %s", index+1, total, repo.Name, repo.URL)

		repoIndex, err := loadRepoIndex(ctx, dctx.Profile.PackageType, repo)
		if err != nil {
			updatePreloadStatus(func(state *preloadSnapshot) {
				state.ReposDone++
				state.Message = fmt.Sprintf("仓库加载失败：%s", repo.Name)
			})
			log.Printf("preload repo %d/%d failed after %s: %s %s", index+1, total, time.Since(startedAt).Round(time.Second), repo.Name, err)
			errors = append(errors, manifestErr{Repo: repo.URL, Message: err.Error()})
			waitBetweenPreloadRepos(ctx, index, total)
			continue
		}

		mergeIndex(&combined, repoIndex)
		updatePreloadStatus(func(state *preloadSnapshot) {
			state.ReposDone++
			state.Packages += len(repoIndex.ByName)
			state.Message = fmt.Sprintf("仓库加载完成：%s，%d 个包", repo.Name, len(repoIndex.ByName))
		})
		log.Printf("preload repo %d/%d done: %s packages=%d duration=%s", index+1, total, repo.Name, len(repoIndex.ByName), time.Since(startedAt).Round(time.Second))
		waitBetweenPreloadRepos(ctx, index, total)
	}

	return combined, errors
}

func waitBetweenPreloadRepos(ctx context.Context, index int, total int) {
	if index+1 >= total || config.PreloadRepoPause <= 0 {
		return
	}

	if err := sleepContext(ctx, config.PreloadRepoPause); err != nil {
		log.Printf("preload pause stopped: %v", err)
	}
}

func countPreloadRepos(contexts []downloadContext) int {
	total := 0
	for _, ctx := range contexts {
		total += len(ctx.RepoURLs)
	}
	return total
}

func preloadTargetName(dctx downloadContext) string {
	return fmt.Sprintf("%s / %s / %s", dctx.Profile.Label, dctx.Arch, dctx.Profile.PackageType)
}

func updatePreloadStatus(update func(*preloadSnapshot)) {
	preloadMutex.Lock()
	defer preloadMutex.Unlock()
	update(&preloadState)
}

func currentPreloadStatus() preloadSnapshot {
	preloadMutex.Lock()
	defer preloadMutex.Unlock()
	return preloadState
}

func markPreloadError(err error) {
	if err == nil {
		return
	}

	updatePreloadStatus(func(state *preloadSnapshot) {
		state.Status = "error"
		state.Message = "仓库预热失败"
		state.LastError = err.Error()
		state.FinishedAt = time.Now().Format(time.RFC3339)
	})
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func preloadDownloadContexts() []downloadContext {
	switch config.PreloadMode {
	case "none":
		return nil
	case "all":
		items := []downloadContext{}
		for _, prof := range listProfiles() {
			for _, arch := range prof.Arches {
				values := url.Values{
					"osProfile": {prof.ID},
					"arch":      {arch},
				}
				items = append(items, resolveDownloadContext(values, ""))
			}
		}
		return items
	default:
		prof := profiles[config.DefaultProfileID]
		values := url.Values{
			"osProfile": {prof.ID},
			"arch":      {prof.DefaultArch},
		}
		return []downloadContext{resolveDownloadContext(values, "")}
	}
}

func repoTemplatesFromURLs(urls []string) []repoTemplate {
	repos := make([]repoTemplate, 0, len(urls))
	for i, value := range urls {
		repos = append(repos, repoTemplate{Name: fmt.Sprintf("custom-%d", i+1), URL: value})
	}
	return repos
}

func serveIndexOrStatic(staticFS fs.FS) http.HandlerFunc {
	fileServer := http.FileServer(http.FS(staticFS))
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			data, err := fs.ReadFile(staticFS, "index.html")
			if err != nil {
				http.Error(w, "index page not found", http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(data)
			return
		}

		fileServer.ServeHTTP(w, r)
	}
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	ctx := resolveDownloadContext(r.URL.Query(), r.UserAgent())

	writeJSON(w, map[string]any{
		"selected": map[string]any{
			"osProfile":   ctx.Profile.ID,
			"osLabel":     ctx.Profile.Label,
			"packageType": ctx.Profile.PackageType,
			"arch":        ctx.Arch,
			"repos":       ctx.RepoURLs,
		},
		"detected":       ctx.Client,
		"profiles":       listProfiles(),
		"indexes":        listProfileIndexStates(),
		"maxPackages":    config.MaxPackages,
		"maxResolved":    config.MaxResolved,
		"allowDirectURL": config.AllowDirectURL,
		"preload":        currentPreloadStatus(),
	})
}

func handleIndexStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, listProfileIndexStates())
}

func handleOSDetect(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, detectOperatingSystemFromUserAgent(r.UserAgent()))
}

func handlePreloadStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, currentPreloadStatus())
}

func handlePreloadStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	dctx := resolveDownloadContext(r.Form, r.UserAgent())
	_ = ensureProfileRepos(context.Background(), dctx)
	updatePreloadStatus(func(state *preloadSnapshot) {
		*state = preloadSnapshot{
			Mode:          "manual",
			Blocking:      false,
			Status:        "waiting",
			Message:       "仓库索引已加入加载任务",
			CurrentTarget: preloadTargetName(dctx),
			TargetsTotal:  1,
			ReposTotal:    len(dctx.RepoURLs),
		}
	})

	writeJSON(w, profileIndexStateForContext(dctx))
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	requested := parsePackageInput(firstValue(r.Form, "packages", "packageName", "q"))
	includeDeps := formBool(r.Form, "includeDeps", true)
	ctx := resolveDownloadContext(r.Form, r.UserAgent())

	if len(requested) == 0 {
		http.Error(w, "please input at least one package name or package url", http.StatusBadRequest)
		return
	}

	if len(requested) > config.MaxPackages {
		http.Error(w, fmt.Sprintf("too many packages, max is %d", config.MaxPackages), http.StatusBadRequest)
		return
	}

	startedAt := time.Now().UTC()
	filename := zipFilenameFromRequests(requested)
	man := manifest{
		StartedAt:       startedAt.Format(time.RFC3339),
		Requested:       requested,
		IncludeDeps:     includeDeps,
		DependencyLimit: config.MaxResolved,
		Target: manifestTarget{
			OSProfile:   ctx.Profile.ID,
			OSLabel:     ctx.Profile.Label,
			PackageType: ctx.Profile.PackageType,
			Arch:        ctx.Arch,
			Repos:       ctx.RepoURLs,
		},
		Client: ctx.Client,
		Files:  []manifestFile{},
		Errors: []manifestErr{},
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition(filename))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")

	zipWriter := zip.NewWriter(w)
	defer zipWriter.Close()

	writeZipText(zipWriter, "README.txt", strings.Join([]string{
		"Package ZIP download started.",
		"Started at: " + startedAt.Format(time.RFC3339),
		"Target OS: " + ctx.Profile.Label,
		"Package type: " + string(ctx.Profile.PackageType),
		"Target arch: " + ctx.Arch,
		"Include dependencies: " + strconv.FormatBool(includeDeps),
		"Requested: " + strings.Join(requested, ", "),
		"",
		"Repository metadata is resolved after this ZIP starts streaming.",
		"Files are streamed into this ZIP as they are downloaded.",
	}, "\n"))
	flush(w)

	files := resolveRequestedFiles(r.Context(), requested, includeDeps, ctx, &man)
	man.ResolvedCount = len(files)
	if len(files) == 0 && len(man.Errors) == 0 {
		man.Errors = append(man.Errors, manifestErr{Message: "没有找到可下载的包文件"})
	}

	for _, file := range files {
		appendRemotePackage(r.Context(), zipWriter, w, &man, file)
	}

	writeManifest(zipWriter, &man)
	flush(w)
}

func resolveRequestedFiles(ctx context.Context, requested []string, includeDeps bool, dctx downloadContext, man *manifest) []remoteFile {
	files := []remoteFile{}
	seenURLs := map[string]bool{}

	for _, item := range requested {
		if isDirectPackageURL(item) {
			file, err := directRemoteFile(item)
			if err != nil {
				man.Errors = append(man.Errors, manifestErr{Requested: item, Source: item, Message: err.Error()})
				continue
			}
			if !config.AllowDirectURL {
				man.Errors = append(man.Errors, manifestErr{Requested: item, Source: item, Message: "direct package url download is disabled"})
				continue
			}
			if !seenURLs[file.URL] {
				seenURLs[file.URL] = true
				files = append(files, file)
			}
			continue
		}

		result := resolvePackageClosure(ctx, item, includeDeps, dctx)
		man.Errors = append(man.Errors, result.RepoErrors...)
		if len(result.Packages) == 0 {
			man.Errors = append(man.Errors, manifestErr{
				Package: item,
				Message: fmt.Sprintf("package not found in %s (%s): %s", dctx.Profile.Label, dctx.Arch, item),
			})
			continue
		}

		for _, pkg := range result.Packages {
			if seenURLs[pkg.Source] {
				continue
			}
			seenURLs[pkg.Source] = true
			pkgCopy := pkg
			files = append(files, remoteFile{
				Requested: item,
				URL:       pkg.Source,
				Filename:  pkg.Filename,
				Repo:      pkg.Repo,
				Package:   &pkgCopy,
			})
		}
	}

	return files
}

func resolvePackageClosure(ctx context.Context, packageName string, includeDeps bool, dctx downloadContext) resolvedPackage {
	index, repoErrors := loadCombinedIndex(ctx, dctx)
	result := resolvedPackage{RepoErrors: repoErrors}
	root, ok := findBestPackage(index, packageName, dctx.Arches, dctx.Profile.PackageType)
	if !ok {
		return result
	}

	ordered := []packageMeta{}
	seenPackage := map[string]bool{}
	queue := []packageMeta{root}

	for len(queue) > 0 {
		if len(ordered) >= config.MaxResolved {
			result.RepoErrors = append(result.RepoErrors, manifestErr{
				Package: packageName,
				Message: fmt.Sprintf("dependency limit reached: %d", config.MaxResolved),
			})
			break
		}

		current := queue[0]
		queue = queue[1:]
		key := packageKey(current)
		if seenPackage[key] {
			continue
		}
		seenPackage[key] = true
		ordered = append(ordered, current)

		if !includeDeps {
			continue
		}

		for _, dep := range dependencyNames(current, dctx.Profile.PackageType) {
			if isIgnoredDependency(dep) {
				continue
			}
			next, ok := findBestPackage(index, dep, dctx.Arches, dctx.Profile.PackageType)
			if !ok {
				result.RepoErrors = append(result.RepoErrors, manifestErr{
					Package: current.Name,
					Message: fmt.Sprintf("dependency not found: %s", dep),
				})
				continue
			}
			queue = append(queue, next)
		}
	}

	result.Packages = ordered
	return result
}

func loadCombinedIndex(ctx context.Context, dctx downloadContext) (packageIndex, []manifestErr) {
	combined := newPackageIndex()
	errors := []manifestErr{}

	for _, repo := range dctx.RepoURLs {
		index, err := loadRepoIndex(ctx, dctx.Profile.PackageType, repo)
		if err != nil {
			errors = append(errors, manifestErr{Repo: repo.URL, Message: err.Error()})
			continue
		}
		mergeIndex(&combined, index)
	}

	return combined, errors
}

func loadRepoIndex(ctx context.Context, kind repoKind, repo repoInfo) (packageIndex, error) {
	cacheKey := string(kind) + "|" + repo.URL
	cacheMutex.Lock()
	cached, ok := repoCache[cacheKey]
	if ok && time.Since(cached.LoadedAt) < config.CacheTTL {
		cacheMutex.Unlock()
		return cached.Index, nil
	}
	if call, ok := repoLoads[cacheKey]; ok {
		cacheMutex.Unlock()
		log.Printf("waiting for repository index already loading: %s %s", repo.Name, repo.URL)
		select {
		case <-ctx.Done():
			return packageIndex{}, ctx.Err()
		case <-call.Done:
			if call.Err != nil {
				return packageIndex{}, call.Err
			}
			return call.Index, nil
		}
	}

	call := &repoLoadCall{Done: make(chan struct{})}
	repoLoads[cacheKey] = call
	cacheMutex.Unlock()

	var (
		index packageIndex
		err   error
	)

	startedAt := time.Now()
	log.Printf("loading repository index: %s %s", repo.Name, repo.URL)
	switch kind {
	case repoKindRPM:
		index, err = fetchRPMRepoIndex(ctx, repo)
	case repoKindDEB:
		index, err = fetchDEBRepoIndex(ctx, repo)
	default:
		err = fmt.Errorf("unsupported repo kind: %s", kind)
	}

	cacheMutex.Lock()
	if err == nil {
		repoCache[cacheKey] = repoCacheEntry{LoadedAt: time.Now(), Index: index}
	}
	call.Index = index
	call.Err = err
	delete(repoLoads, cacheKey)
	close(call.Done)
	cacheMutex.Unlock()

	if err != nil {
		log.Printf("repository index failed after %s: %s %s", time.Since(startedAt).Round(time.Second), repo.Name, err)
		return packageIndex{}, err
	}

	log.Printf("repository index loaded: %s packages=%d duration=%s", repo.Name, len(index.ByName), time.Since(startedAt).Round(time.Second))
	return index, nil
}

func fetchRPMRepoIndex(ctx context.Context, repo repoInfo) (packageIndex, error) {
	repoURL := ensureTrailingSlash(repo.URL)
	repomdURL := resolveURL(repoURL, "repodata/repomd.xml")
	repomdBody, err := fetchIndexBytes(ctx, repomdURL)
	if err != nil {
		return packageIndex{}, err
	}

	primaryHref, err := extractPrimaryHref(repomdBody)
	if err != nil {
		return packageIndex{}, err
	}

	primaryURL := resolveURL(repoURL, primaryHref)
	resp, cancel, err := fetchIndexResponse(ctx, primaryURL)
	if err != nil {
		return packageIndex{}, err
	}
	defer cancel()
	defer resp.Body.Close()

	var reader io.Reader = resp.Body
	if strings.HasSuffix(strings.ToLower(primaryURL), ".gz") {
		gzipReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return packageIndex{}, err
		}
		defer gzipReader.Close()
		reader = gzipReader
	}

	return parseRPMPrimaryXML(reader, repo)
}

func fetchDEBRepoIndex(ctx context.Context, repo repoInfo) (packageIndex, error) {
	resp, cancel, err := fetchIndexResponse(ctx, repo.URL)
	if err != nil {
		return packageIndex{}, err
	}
	defer cancel()
	defer resp.Body.Close()

	var reader io.Reader = resp.Body
	if strings.HasSuffix(strings.ToLower(repo.URL), ".gz") {
		gzipReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return packageIndex{}, err
		}
		defer gzipReader.Close()
		reader = gzipReader
	}

	return parseDEBPackages(reader, repo)
}

func fetchIndexBytes(ctx context.Context, remoteURL string) ([]byte, error) {
	resp, cancel, err := fetchIndexResponse(ctx, remoteURL)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

func fetchIndexResponse(ctx context.Context, remoteURL string) (*http.Response, context.CancelFunc, error) {
	return fetchResponseWithTimeout(ctx, remoteURL, config.IndexTimeout)
}

func fetchResponse(ctx context.Context, remoteURL string) (*http.Response, context.CancelFunc, error) {
	return fetchResponseWithTimeout(ctx, remoteURL, config.RequestTimeout)
}

func fetchResponseWithTimeout(ctx context.Context, remoteURL string, timeout time.Duration) (*http.Response, context.CancelFunc, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, remoteURL, nil)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "package-down/1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		cancel()
		return nil, nil, fmt.Errorf("request failed: %s http %d", remoteURL, resp.StatusCode)
	}

	return resp, cancel, nil
}

func appendRemotePackage(ctx context.Context, zipWriter *zip.Writer, w http.ResponseWriter, man *manifest, file remoteFile) {
	reqCtx, cancel := context.WithTimeout(ctx, config.RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, file.URL, nil)
	if err != nil {
		appendDownloadError(zipWriter, w, man, file, err)
		return
	}
	req.Header.Set("User-Agent", "package-down/1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		appendDownloadError(zipWriter, w, man, file, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		appendDownloadError(zipWriter, w, man, file, fmt.Errorf("download failed: http %d", resp.StatusCode))
		return
	}

	entryName := packageFolder(man.Target.PackageType) + "/" + safeZipName(file.Filename)
	writer, err := zipWriter.Create(entryName)
	if err != nil {
		appendDownloadError(zipWriter, w, man, file, err)
		return
	}

	if _, err := io.Copy(writer, resp.Body); err != nil {
		appendDownloadError(zipWriter, w, man, file, err)
		return
	}

	var size *int64
	if resp.ContentLength >= 0 {
		value := resp.ContentLength
		size = &value
	}

	man.Files = append(man.Files, manifestFile{
		Requested: file.Requested,
		Name:      entryName,
		Source:    file.URL,
		Repo:      file.Repo,
		Size:      size,
		Package:   file.Package,
	})
	flush(w)
}

func appendDownloadError(zipWriter *zip.Writer, w http.ResponseWriter, man *manifest, file remoteFile, err error) {
	message := err.Error()
	man.Errors = append(man.Errors, manifestErr{
		Requested: file.Requested,
		Source:    file.URL,
		Message:   message,
	})
	writeZipText(zipWriter, "errors/"+safeZipName(file.Requested)+".txt", message+"\n"+file.URL+"\n")
	flush(w)
}

func extractPrimaryHref(data []byte) (string, error) {
	type location struct {
		Href string `xml:"href,attr"`
	}
	type repoData struct {
		Type     string   `xml:"type,attr"`
		Location location `xml:"location"`
	}
	type repoMD struct {
		Data []repoData `xml:"data"`
	}

	var doc repoMD
	if err := xml.Unmarshal(data, &doc); err != nil {
		return "", err
	}

	for _, item := range doc.Data {
		if item.Type == "primary" && item.Location.Href != "" {
			return item.Location.Href, nil
		}
	}

	return "", errors.New("primary metadata not found")
}

func parseRPMPrimaryXML(reader io.Reader, repo repoInfo) (packageIndex, error) {
	decoder := xml.NewDecoder(reader)
	index := newPackageIndex()
	var current *packageMeta
	var currentRels *[]string

	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return packageIndex{}, err
		}

		switch item := token.(type) {
		case xml.StartElement:
			if item.Name.Local == "package" && attrValue(item.Attr, "type") == "rpm" {
				current = &packageMeta{PackageType: repoKindRPM, Repo: repo.Name}
				continue
			}

			if current == nil {
				continue
			}

			switch item.Name.Local {
			case "name":
				current.Name = readElementText(decoder, item)
			case "arch":
				current.Arch = readElementText(decoder, item)
			case "version":
				current.Epoch = defaultString(attrValue(item.Attr, "epoch"), "0")
				current.Version = attrValue(item.Attr, "ver")
				current.Release = attrValue(item.Attr, "rel")
			case "location":
				current.Filename = attrValue(item.Attr, "href")
				current.Source = resolveURL(repo.URL, current.Filename)
			case "provides":
				currentRels = &current.Provides
			case "requires":
				currentRels = &current.Requires
			case "entry":
				if currentRels != nil {
					name := normalizeRPMDependency(attrValue(item.Attr, "name"))
					if name != "" {
						*currentRels = appendUnique(*currentRels, name)
					}
				}
			case "file":
				name := readElementText(decoder, item)
				if name != "" {
					current.Provides = appendUnique(current.Provides, name)
				}
			}

		case xml.EndElement:
			switch item.Name.Local {
			case "provides", "requires":
				currentRels = nil
			case "package":
				if current != nil && current.Name != "" && current.Arch != "" && current.Source != "" {
					addPackageToIndex(&index, *current)
				}
				current = nil
			}
		}
	}

	return index, nil
}

func parseDEBPackages(reader io.Reader, repo repoInfo) (packageIndex, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 1024), 16*1024*1024)
	index := newPackageIndex()
	fields := map[string]string{}
	lastKey := ""

	flushPackage := func() {
		if len(fields) == 0 {
			return
		}
		pkg := packageMeta{
			Name:        fields["Package"],
			PackageType: repoKindDEB,
			Arch:        fields["Architecture"],
			Version:     fields["Version"],
			Filename:    fields["Filename"],
			Repo:        repo.Name,
			Depends:     parseDEBDependencyNames(fields["Depends"], fields["Pre-Depends"]),
		}
		if pkg.Name != "" && pkg.Filename != "" {
			pkg.Source = resolveURL(debRepoBase(repo.URL), pkg.Filename)
			pkg.Provides = appendUnique(pkg.Provides, pkg.Name)
			pkg.Provides = appendUnique(pkg.Provides, parseDEBProvides(fields["Provides"])...)
			addPackageToIndex(&index, pkg)
		}
		fields = map[string]string{}
		lastKey = ""
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flushPackage()
			continue
		}
		if strings.HasPrefix(line, " ") && lastKey != "" {
			fields[lastKey] += "\n" + strings.TrimSpace(line)
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		lastKey = key
		fields[key] = strings.TrimSpace(value)
	}
	flushPackage()

	if err := scanner.Err(); err != nil {
		return packageIndex{}, err
	}

	return index, nil
}

func readElementText(decoder *xml.Decoder, start xml.StartElement) string {
	var text string
	if err := decoder.DecodeElement(&text, &start); err != nil {
		return ""
	}
	return strings.TrimSpace(text)
}

func resolveDownloadContext(values url.Values, userAgent string) downloadContext {
	profileID := firstValue(values, "osProfile", "os")
	if profileID == "" {
		profileID = config.DefaultProfileID
	}

	prof, ok := profiles[profileID]
	if !ok {
		prof = profiles[config.DefaultProfileID]
	}

	arch := normalizeTargetArchForProfile(firstValue(values, "arch"), prof)
	if arch == "" {
		arch = prof.DefaultArch
	}

	return downloadContext{
		Profile:  prof,
		Arch:     arch,
		Arches:   supportedArches(prof.PackageType, arch),
		RepoURLs: buildRepoURLs(prof, arch),
		Client: clientInfo{
			BrowserReported: reportedClient{
				OS:   cleanShortText(firstValue(values, "clientOs")),
				Arch: normalizeClientArch(firstValue(values, "clientArch")),
			},
			ServerDetected: detectOperatingSystemFromUserAgent(userAgent),
		},
	}
}

func buildRepoURLs(prof profile, arch string) []repoInfo {
	repos := []repoInfo{}
	for _, repo := range prof.Repos {
		if len(repo.Tags) > 0 && !contains(repo.Tags, arch) {
			continue
		}
		value := strings.ReplaceAll(repo.URL, "{arch}", arch)
		value = strings.ReplaceAll(value, "{centos7arch}", centos7ArchPath(arch))
		repos = append(repos, repoInfo{
			Name: repo.Name,
			URL:  ensureRepoURL(prof.PackageType, value),
			Tags: repo.Tags,
		})
	}
	return repos
}

func listProfiles() []profile {
	items := make([]profile, 0, len(profiles))
	for _, prof := range profiles {
		items = append(items, prof)
	}
	sort.Slice(items, func(i, j int) bool {
		leftOrder := profileDisplayOrder(items[i].ID)
		rightOrder := profileDisplayOrder(items[j].ID)
		if leftOrder != rightOrder {
			return leftOrder < rightOrder
		}
		return items[i].Label < items[j].Label
	})
	return items
}

func profileDisplayOrder(id string) int {
	switch id {
	case "ubuntu-20.04":
		return 10
	case "ubuntu-22.04":
		return 11
	case "ubuntu-24.04":
		return 12
	case "centos-7":
		return 20
	case "centos-stream-9":
		return 21
	case "almalinux-8":
		return 30
	case "almalinux-9":
		return 31
	case "rocky-9":
		return 32
	case "kylin-v10-sp3-2403":
		return 33
	case "custom":
		return 40
	default:
		return 100
	}
}

func detectOperatingSystemFromUserAgent(userAgent string) detectedClient {
	value := userAgent
	ua := strings.ToLower(userAgent)
	osName := "unknown"

	switch {
	case strings.Contains(ua, "android"):
		osName = "Android"
	case strings.Contains(ua, "iphone"), strings.Contains(ua, "ipad"), strings.Contains(ua, "ipod"):
		osName = "iOS"
	case strings.Contains(ua, "windows nt 10.0"):
		osName = "Windows 10/11"
	case strings.Contains(ua, "windows"):
		osName = "Windows"
	case strings.Contains(ua, "mac os x"):
		osName = "macOS"
	case strings.Contains(ua, "linux"):
		osName = "Linux"
	}

	return detectedClient{
		OS:        osName,
		Arch:      normalizeClientArch(value),
		UserAgent: value,
	}
}

func newPackageIndex() packageIndex {
	return packageIndex{
		ByName:    map[string][]packageMeta{},
		Providers: map[string][]packageMeta{},
	}
}

func mergeIndex(target *packageIndex, source packageIndex) {
	for _, packages := range source.ByName {
		for _, pkg := range packages {
			addPackageToIndex(target, pkg)
		}
	}
}

func addPackageToIndex(index *packageIndex, pkg packageMeta) {
	index.ByName[pkg.Name] = append(index.ByName[pkg.Name], pkg)
	provides := append([]string{pkg.Name}, pkg.Provides...)
	for _, provide := range provides {
		provide = normalizeDependencyName(provide, pkg.PackageType)
		if provide == "" {
			continue
		}
		index.Providers[provide] = append(index.Providers[provide], pkg)
	}
}

func findBestPackage(index packageIndex, name string, arches map[string]bool, kind repoKind) (packageMeta, bool) {
	name = normalizeDependencyName(name, kind)
	candidates := append([]packageMeta{}, index.ByName[name]...)
	candidates = append(candidates, index.Providers[name]...)

	filtered := make([]packageMeta, 0, len(candidates))
	seen := map[string]bool{}
	for _, item := range candidates {
		key := packageKey(item)
		if seen[key] || !arches[item.Arch] {
			continue
		}
		seen[key] = true
		filtered = append(filtered, item)
	}

	if len(filtered) == 0 {
		return packageMeta{}, false
	}

	sort.Slice(filtered, func(i, j int) bool {
		return comparePackages(filtered[i], filtered[j]) < 0
	})

	return filtered[len(filtered)-1], true
}

func comparePackages(left, right packageMeta) int {
	if left.PackageType == repoKindDEB || right.PackageType == repoKindDEB {
		if result := debVersionCompare(left.Version, right.Version); result != 0 {
			return result
		}
		return archPreference(left.Arch) - archPreference(right.Arch)
	}

	if result := compareNumberStrings(left.Epoch, right.Epoch); result != 0 {
		return result
	}
	if result := rpmLabelCompare(left.Version, right.Version); result != 0 {
		return result
	}
	if result := rpmLabelCompare(left.Release, right.Release); result != 0 {
		return result
	}
	return archPreference(left.Arch) - archPreference(right.Arch)
}

func dependencyNames(pkg packageMeta, kind repoKind) []string {
	switch kind {
	case repoKindDEB:
		return pkg.Depends
	default:
		return pkg.Requires
	}
}

func parseDEBDependencyNames(values ...string) []string {
	result := []string{}
	for _, value := range values {
		if value == "" {
			continue
		}
		for _, group := range debDepSplit.Split(value, -1) {
			alternatives := debAltSplit.Split(group, -1)
			for _, alternative := range alternatives {
				name := normalizeDEBDependency(alternative)
				if name != "" {
					result = appendUnique(result, name)
					break
				}
			}
		}
	}
	return result
}

func parseDEBProvides(value string) []string {
	result := []string{}
	for _, group := range debDepSplit.Split(value, -1) {
		name := normalizeDEBDependency(group)
		if name != "" {
			result = appendUnique(result, name)
		}
	}
	return result
}

func normalizeDEBDependency(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if before, _, ok := strings.Cut(value, " "); ok {
		value = before
	}
	if before, _, ok := strings.Cut(value, "("); ok {
		value = before
	}
	if before, _, ok := strings.Cut(value, ":"); ok {
		value = before
	}
	return strings.TrimSpace(value)
}

func normalizeRPMDependency(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return rpmDepNameRe.FindString(value)
}

func normalizeDependencyName(value string, kind repoKind) string {
	if kind == repoKindDEB {
		return normalizeDEBDependency(value)
	}
	return normalizeRPMDependency(value)
}

func isIgnoredDependency(value string) bool {
	if value == "" {
		return true
	}
	if strings.HasPrefix(value, "rpmlib(") || strings.HasPrefix(value, "config(") {
		return true
	}
	if strings.HasPrefix(value, "module(") {
		return true
	}
	return false
}

func archPreference(arch string) int {
	switch arch {
	case "noarch", "all":
		return 0
	default:
		return 1
	}
}

func rpmLabelCompare(left, right string) int {
	leftTokens := tokenRe.FindAllString(left, -1)
	rightTokens := tokenRe.FindAllString(right, -1)
	maxLen := len(leftTokens)
	if len(rightTokens) > maxLen {
		maxLen = len(rightTokens)
	}

	for i := 0; i < maxLen; i++ {
		if i >= len(leftTokens) {
			return -1
		}
		if i >= len(rightTokens) {
			return 1
		}

		a := leftTokens[i]
		b := rightTokens[i]
		aNum := isNumber(a)
		bNum := isNumber(b)

		if aNum != bNum {
			if aNum {
				return 1
			}
			return -1
		}

		if aNum {
			if result := compareNumberStrings(a, b); result != 0 {
				return result
			}
			continue
		}

		if result := strings.Compare(a, b); result != 0 {
			return result
		}
	}

	return 0
}

func debVersionCompare(left, right string) int {
	return rpmLabelCompare(left, right)
}

func compareNumberStrings(left, right string) int {
	a := strings.TrimLeft(defaultString(left, "0"), "0")
	b := strings.TrimLeft(defaultString(right, "0"), "0")
	if a == "" {
		a = "0"
	}
	if b == "" {
		b = "0"
	}

	if len(a) > len(b) {
		return 1
	}
	if len(a) < len(b) {
		return -1
	}
	return strings.Compare(a, b)
}

func parsePackageInput(input string) []string {
	fields := strings.FieldsFunc(input, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})

	seen := map[string]bool{}
	items := []string{}
	for _, field := range fields {
		value := strings.TrimSpace(field)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		items = append(items, value)
	}
	return items
}

func writeZipText(zipWriter *zip.Writer, name string, value string) {
	writer, err := zipWriter.Create(name)
	if err != nil {
		return
	}
	_, _ = writer.Write([]byte(value))
}

func writeManifest(zipWriter *zip.Writer, man *manifest) {
	data, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		writeZipText(zipWriter, "manifest-error.txt", err.Error())
		return
	}
	writeZipText(zipWriter, "manifest.json", string(data)+"\n")
}

func downloadErrorText(man manifest) string {
	var builder strings.Builder
	builder.WriteString("没有找到可下载的包文件。\n")
	builder.WriteString("请求包名: " + strings.Join(man.Requested, ", ") + "\n")
	builder.WriteString("目标系统: " + man.Target.OSLabel + "\n")
	builder.WriteString("目标架构: " + man.Target.Arch + "\n")
	builder.WriteString("包类型: " + string(man.Target.PackageType) + "\n")
	if len(man.Errors) > 0 {
		builder.WriteString("\n错误详情:\n")
		for _, item := range man.Errors {
			if item.Package != "" {
				builder.WriteString("- " + item.Package + ": " + item.Message + "\n")
				continue
			}
			if item.Repo != "" {
				builder.WriteString("- " + item.Repo + ": " + item.Message + "\n")
				continue
			}
			builder.WriteString("- " + item.Message + "\n")
		}
	}
	return builder.String()
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

func flush(w http.ResponseWriter) {
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func isDirectPackageURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	lower := strings.ToLower(parsed.Path)
	return strings.HasSuffix(lower, ".rpm") || strings.HasSuffix(lower, ".deb")
}

func directRemoteFile(value string) (remoteFile, error) {
	if !isDirectPackageURL(value) {
		return remoteFile{}, errors.New("not a direct package url")
	}
	return remoteFile{
		Requested: value,
		URL:       value,
		Filename:  filenameFromURL(value),
	}, nil
}

func filenameFromURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return "package"
	}
	name, err := url.PathUnescape(path.Base(parsed.Path))
	if err != nil || name == "." || name == "/" || name == "" {
		return "package"
	}
	return name
}

func safeZipName(value string) string {
	name := filenameFromURL(value)
	if !strings.Contains(value, "://") {
		name = path.Base(strings.ReplaceAll(value, "\\", "/"))
	}

	var builder strings.Builder
	for _, r := range name {
		if r == '.' || r == '+' || r == '@' || r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' {
			builder.WriteRune(r)
			continue
		}
		builder.WriteRune('_')
	}

	result := builder.String()
	if result == "" {
		return "package"
	}
	if len(result) > 180 {
		return result[:180]
	}
	return result
}

func zipFilenameFromRequests(requested []string) string {
	names := make([]string, 0, len(requested))
	for _, item := range requested {
		name := item
		if isDirectPackageURL(item) {
			name = strings.TrimSuffix(filenameFromURL(item), path.Ext(filenameFromURL(item)))
		}
		name = safeBaseName(name)
		if name != "" {
			names = append(names, name)
		}
	}

	if len(names) == 0 {
		return "packages.zip"
	}

	base := strings.Join(names, "_")
	if len(base) > 160 {
		base = base[:160]
	}
	return base + ".zip"
}

func safeBaseName(value string) string {
	value = path.Base(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"))
	value = strings.TrimSuffix(value, path.Ext(value))

	var builder strings.Builder
	for _, r := range value {
		if r == '.' || r == '+' || r == '@' || r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' {
			builder.WriteRune(r)
			continue
		}
		builder.WriteRune('_')
	}

	return strings.Trim(builder.String(), "_")
}

func packageFolder(kind repoKind) string {
	if kind == repoKindDEB {
		return "debs"
	}
	return "rpms"
}

func contentDisposition(filename string) string {
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, filename, url.PathEscape(filename))
}

func resolveURL(baseURL string, ref string) string {
	base, err := url.Parse(ensureTrailingSlash(baseURL))
	if err != nil {
		return ref
	}
	resolved, err := base.Parse(ref)
	if err != nil {
		return ref
	}
	return resolved.String()
}

func debRepoBase(packagesURL string) string {
	parsed, err := url.Parse(packagesURL)
	if err != nil {
		return packagesURL
	}
	marker := "/dists/"
	index := strings.Index(parsed.Path, marker)
	if index < 0 {
		return packagesURL
	}
	parsed.Path = parsed.Path[:index+1]
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func ensureRepoURL(kind repoKind, value string) string {
	if kind == repoKindDEB {
		return value
	}
	return ensureTrailingSlash(value)
}

func ensureTrailingSlash(value string) string {
	if strings.HasSuffix(value, "/") {
		return value
	}
	return value + "/"
}

func normalizeTargetArchForProfile(value string, prof profile) string {
	if prof.PackageType == repoKindDEB {
		return normalizeDEBArch(value)
	}
	return normalizeRPMArch(value)
}

func normalizeTargetArchOrDefault(value string, fallback string) string {
	arch := normalizeRPMArch(value)
	if arch == "" {
		return fallback
	}
	return arch
}

func normalizeRPMArch(value string) string {
	text := strings.ToLower(strings.TrimSpace(value))
	switch text {
	case "arm64", "aarch64":
		return "aarch64"
	case "amd64", "x64", "x86_64":
		return "x86_64"
	default:
		return ""
	}
}

func normalizeDEBArch(value string) string {
	text := strings.ToLower(strings.TrimSpace(value))
	switch text {
	case "arm64", "aarch64":
		return "arm64"
	case "amd64", "x64", "x86_64":
		return "amd64"
	default:
		return ""
	}
}

func supportedArches(kind repoKind, arch string) map[string]bool {
	if kind == repoKindDEB {
		return map[string]bool{arch: true, "all": true}
	}
	return map[string]bool{arch: true, "noarch": true}
}

func normalizeClientArch(value string) string {
	text := strings.ToLower(value)
	switch {
	case strings.Contains(text, "aarch64"), strings.Contains(text, "arm64"):
		return "arm64"
	case strings.Contains(text, "x86_64"), strings.Contains(text, "x64"), strings.Contains(text, "amd64"), strings.Contains(text, "win64"), strings.Contains(text, "wow64"):
		return "x64"
	case strings.Contains(text, "i386"), strings.Contains(text, "i686"), strings.Contains(text, "x86"):
		return "x86"
	default:
		if text == "" {
			return "unknown"
		}
		return cleanShortText(text)
	}
}

func cleanShortText(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	for _, r := range value {
		if r == ' ' || r == '.' || r == '+' || r == '/' || r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' {
			builder.WriteRune(r)
		}
		if builder.Len() >= 80 {
			break
		}
	}
	return builder.String()
}

func centos7ArchPath(arch string) string {
	if arch == "aarch64" {
		return "altarch/7.9.2009"
	}
	return "7.9.2009"
}

func packageKey(pkg packageMeta) string {
	return string(pkg.PackageType) + "|" + pkg.Name + "|" + pkg.Arch + "|" + pkg.Version + "|" + pkg.Release + "|" + pkg.Source
}

func attrValue(attrs []xml.Attr, name string) string {
	for _, attr := range attrs {
		if attr.Name.Local == name {
			return attr.Value
		}
	}
	return ""
}

func firstValue(values url.Values, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(values.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func formBool(values url.Values, name string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(values.Get(name)))
	if value == "" {
		return fallback
	}
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func normalizePreloadMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none", "off", "false", "0":
		return "none"
	case "all":
		return "all"
	default:
		return "default"
	}
}

func splitList(value string) []string {
	parts := strings.Split(value, ",")
	items := []string{}
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item != "" {
			items = append(items, item)
		}
	}
	return items
}

func appendUnique(items []string, values ...string) []string {
	seen := map[string]bool{}
	for _, item := range items {
		seen[item] = true
	}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		items = append(items, value)
	}
	return items
}

func contains(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func envBool(name string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if value == "" {
		return fallback
	}
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value <= 0 {
		return fallback
	}
	return time.Duration(value) * time.Millisecond
}

func envDurationAllowZero(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return fallback
	}
	return time.Duration(value) * time.Millisecond
}

func getenv(name string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func defaultString(value string, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func isNumber(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

func logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %q %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
