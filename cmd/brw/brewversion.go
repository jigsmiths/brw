package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/relocate"
	"github.com/jigsmiths/brw/internal/tap"
)

func git(repo string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = tap.GitEnv()
	return cmd
}

const noRepositoryVersion = ">=4.3.0 (shallow or no git repository)"

// brew reads a receipt's runtime_dependencies only from Homebrew 1.1.6, so brw's own version won't do.
func homebrewVersion(prefix string) string {
	repo := relocate.ForPrefix(prefix).Repository
	if v := cachedVersion(repo); v != "" {
		return v
	}
	if out, err := git(repo, "describe", "--tags", "--dirty", "--abbrev=7").Output(); err == nil {
		if v := strings.TrimSpace(string(out)); v != "" {
			return v
		}
	}
	kegs, _ := cellar.Prefix(prefix).Kegs()
	newest := ""
	for _, k := range kegs {
		if k.Receipt == nil || k.Receipt.HomebrewVersion == "" {
			continue
		}
		if v := k.Receipt.HomebrewVersion; newest == "" || olderVersion(newest, v) {
			newest = v
		}
	}
	if newest != "" {
		return newest
	}
	return noRepositoryVersion
}

// cachedVersion reads the describe cache of brew's set-homebrew-version-from-git; a dirty tree misses it.
func cachedVersion(repo string) string {
	out, err := git(repo, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(repo, ".git", "describe-cache", strings.TrimSpace(string(out))))
	if err != nil {
		return ""
	}
	v, _, _ := strings.Cut(string(b), "\n")
	if v == "" || strings.HasSuffix(v, "-dirty") {
		return ""
	}
	devCmdRun, _ := git(repo, "config", "--get", "homebrew.devcmdrun").Output()
	developer := os.Getenv("HOMEBREW_DEVELOPER") != "" || os.Getenv("HOMEBREW_DEV_CMD_RUN") != "" ||
		strings.TrimSpace(string(devCmdRun)) == "true"
	if developer && git(repo, "diff", "--quiet", "--no-ext-diff").Run() != nil {
		return ""
	}
	return v
}
