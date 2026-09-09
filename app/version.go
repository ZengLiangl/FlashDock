package app

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"FlashDock/data"
)

// Version 当前应用版本（不含 v 前缀）。可通过构建 ldflags 注入：
//
//	-ldflags "-X FlashDock/app.Version=1.3.0"
//
// 默认与 wails.json info.productVersion 保持一致。
var Version = "1.1.41"

// GitHubToken 可选：构建时注入，用于提高公开 API 速率限制（仓库已公开，检查更新不再必需）。
// 也可运行时设 FLASHDOCK_GITHUB_TOKEN，或本地 secrets/github_pat（不入 git）。
var GitHubToken = ""

const (
	githubOwner = "ZengLiangl"
	githubRepo  = "FlashShell"
)

func githubRepoURL() string {
	return "https://github.com/" + githubOwner + "/" + githubRepo
}

func githubReleasesLatestAPIURL() string {
	return "https://api.github.com/repos/" + githubOwner + "/" + githubRepo + "/releases/latest"
}

// GetAppVersion 返回展示用版本号（带 v 前缀）
func (a *App) GetAppVersion() string {
	return formatVersionDisplay(Version)
}

func formatVersionDisplay(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "v0.0.0"
	}
	if strings.HasPrefix(strings.ToLower(v), "v") {
		return "v" + strings.TrimPrefix(strings.ToLower(v), "v")
	}
	return "v" + v
}

func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	// 忽略预发布/构建元数据后缀用于比较
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	return v
}

// compareSemver 返回 1 if a>b, -1 if a<b, 0 if equal / 无法解析时尽量字符串比较
func compareSemver(a, b string) int {
	aa := strings.Split(normalizeVersion(a), ".")
	bb := strings.Split(normalizeVersion(b), ".")
	n := len(aa)
	if len(bb) > n {
		n = len(bb)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(aa) {
			x, _ = strconv.Atoi(aa[i])
		}
		if i < len(bb) {
			y, _ = strconv.Atoi(bb[i])
		}
		if x > y {
			return 1
		}
		if x < y {
			return -1
		}
	}
	return 0
}

func resolveGitHubToken() string {
	if t := strings.TrimSpace(GitHubToken); t != "" {
		return t
	}
	if t := strings.TrimSpace(os.Getenv("FLASHDOCK_GITHUB_TOKEN")); t != "" {
		return t
	}
	// 开发机：项目 secrets/github_pat（已 gitignore）
	candidates := []string{
		filepath.Join("secrets", "github_pat"),
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "secrets", "github_pat"),
			filepath.Join(dir, "..", "secrets", "github_pat"),
		)
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "secrets", "github_pat"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, data.ConfigHomeDirName, "github_pat"),
			filepath.Join(home, data.ConfigHomeDirName, "github_token"),
			filepath.Join(home, data.LegacyConfigHomeDirName, "github_pat"),
			filepath.Join(home, data.LegacyConfigHomeDirName, "github_token"),
		)
	}
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		t := strings.TrimSpace(string(b))
		if t != "" {
			return t
		}
	}
	return ""
}
