package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/install/installtest"
	"github.com/jigsmiths/brw/internal/tap"
)

// headRepo is a project's git repository, as a formula's head URL names, with
// one commit made at a fixed time. It returns the commit.
func headRepo(t *testing.T, echo string) (dir, commit string) {
	t.Helper()
	dir = t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(dir, "tool"), "#!/bin/sh\necho "+echo+"\n")
	if err := os.Chmod(filepath.Join(dir, "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "README.md"), "read me")
	gitIn(t, dir, "add", "-A")
	cmd := exec.Command("git", "-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false",
		"commit", "-q", "-m", echo)
	cmd.Env = append(tap.GitEnv(), "GIT_COMMITTER_DATE=2026-11-23T13:36:25Z", "GIT_AUTHOR_DATE=2026-11-23T13:36:25Z")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	rev := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	rev.Env = tap.GitEnv()
	out, err := rev.Output()
	if err != nil {
		t.Fatal(err)
	}
	return dir, strings.TrimSpace(string(out))
}

func headFormula(url, sum, repo string) string {
	return "class Tool < Formula\n  desc \"A tool with a head\"\n  url \"" + url + "\"\n  sha256 \"" + sum + "\"\n  license \"MIT\"\n" +
		"  head \"file://" + repo + "\", using: :git, branch: \"main\"\n\n" +
		"  def install\n    bin.install \"tool\"\n  end\nend\n"
}

func TestInstallHEADBuildsFromTheRepository(t *testing.T) {
	e, prefix, origin, reg := tapBottleStore(t)
	repo, commit := headRepo(t, "head")
	url, sum := reg.AddSource("tool-1.0.tar.gz", []byte("unused"))
	writeTapFormula(t, origin, "tool", headFormula(url, sum, repo))
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "--HEAD", "u/t/tool"); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	version := "HEAD-" + commit[:7]
	keg := filepath.Join(prefix, "Cellar/tool", version)
	for _, f := range []string{".brew/tool.rb", "INSTALL_RECEIPT.json", "README.md", "bin/tool", "sbom.spdx.json"} {
		if _, err := os.Stat(filepath.Join(keg, f)); err != nil {
			t.Error(err)
		}
	}
	if exists(t, prefix, filepath.Join("Cellar/tool", version, ".git")) {
		t.Error("the repository's .git was installed")
	}
	if target, _ := os.Readlink(filepath.Join(prefix, "bin/tool")); target != "../Cellar/tool/"+version+"/bin/tool" {
		t.Errorf("bin/tool -> %q", target)
	}
	var r struct {
		SourceModifiedTime int64 `json:"source_modified_time"`
		Source             struct {
			Spec        string         `json:"spec"`
			SCMRevision string         `json:"scm_revision"`
			Versions    map[string]any `json:"versions"`
		} `json:"source"`
	}
	b, _ := os.ReadFile(filepath.Join(keg, "INSTALL_RECEIPT.json"))
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if r.Source.Spec != "head" || r.Source.SCMRevision != commit || r.Source.Versions["head"] != version ||
		r.Source.Versions["stable"] != "1.0" || r.SourceModifiedTime != 1795440985 { // the commit's time
		t.Errorf("receipt:\n%s", b)
	}
	sbom, _ := os.ReadFile(filepath.Join(keg, "sbom.spdx.json"))
	for _, want := range []string{`"versionInfo": "` + version + `"`, `"downloadLocation": "file://` + repo + `"`, `"checksums": []`} {
		if !strings.Contains(string(sbom), want) {
			t.Errorf("sbom lacks %s:\n%s", want, sbom)
		}
	}
	cache := filepath.Join(e.client.CacheDir, "tool--git")
	for key, want := range map[string]string{"homebrew.cacheversion": "0", "core.fsmonitor": "false", "advice.detachedhead": "false"} {
		cmd := exec.Command("git", "-C", cache, "config", key)
		cmd.Env = tap.GitEnv()
		if out, err := cmd.Output(); err != nil || strings.TrimSpace(string(out)) != want {
			t.Errorf("cached repository's %s = %q, %v; want %s, as brew's clone has", key, out, err, want)
		}
	}

	// Again: the HEAD keg is there, so nothing is fetched or built.
	e.err.(interface{ Reset() }).Reset()
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "--HEAD", "u/t/tool"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if want := "brw: warning: u/t/tool " + version + " is already installed and up-to-date.\nTo reinstall HEAD, run:\n  brw reinstall tool\n"; !strings.Contains(e.err.(interface{ String() string }).String(), want) {
		t.Errorf("stderr:\n%s\nwant %q", e.err, want)
	}
}

// newCommit commits a new version of the tool to a head repository and
// returns its short commit.
func newCommit(t *testing.T, repo, echo string) string {
	t.Helper()
	writeFile(t, filepath.Join(repo, "tool"), "#!/bin/sh\necho "+echo+"\n")
	gitIn(t, repo, "commit", "-q", "-am", echo)
	cmd := exec.Command("git", "-C", repo, "rev-parse", "--short=7", "HEAD")
	cmd.Env = tap.GitEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// As brew: a HEAD keg is outdated only once --fetch-HEAD sees a new commit;
// upgrade --fetch-HEAD builds it and removes the old HEAD keg; reinstall
// builds the latest commit again.
func TestHEADKegsAreUpgradedAndReinstalledFromTheirRepository(t *testing.T) {
	e, prefix, origin, reg := tapBottleStore(t)
	repo, commit := headRepo(t, "one")
	url, sum := reg.AddSource("tool-1.0.tar.gz", []byte("unused"))
	writeTapFormula(t, origin, "tool", headFormula(url, sum, repo))
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "--HEAD", "u/t/tool"); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	first := "HEAD-" + commit[:7]
	second := "HEAD-" + newCommit(t, repo, "two")
	stderr := func() string { return e.err.(interface{ String() string }).String() }

	if out, err := runCmd(t, e, "outdated", "-prefix", prefix, "--verbose"); err != nil || out != "" {
		t.Errorf("outdated without --fetch-HEAD = %q, %v", out, err)
	}
	if out, err := runCmd(t, e, "outdated", "-prefix", prefix, "--verbose", "--fetch-HEAD"); err != nil ||
		out != "u/t/tool ("+first+") < "+second+"\n" {
		t.Errorf("outdated --fetch-HEAD = %q, %v", out, err)
	}
	if out, err := runCmd(t, e, "outdated", "-prefix", prefix, "--json", "--fetch-HEAD"); err != nil ||
		!strings.Contains(out, `"current_version": "`+second+`"`) {
		t.Errorf("outdated --json --fetch-HEAD = %s, %v", out, err)
	}

	e.err.(interface{ Reset() }).Reset()
	if out, err := runCmd(t, e, "upgrade", "-prefix", prefix, "tool"); err != nil || !strings.Contains(stderr(), "u/t/tool "+first+" already installed") {
		t.Errorf("upgrade without --fetch-HEAD: %v\n%s\n%s", err, out, stderr())
	}
	if out, err := runCmd(t, e, "upgrade", "-prefix", prefix, "--fetch-HEAD", "tool"); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, stderr())
	}
	if exists(t, prefix, "Cellar/tool/"+first) || !exists(t, prefix, "Cellar/tool/"+second) {
		t.Errorf("after upgrade --fetch-HEAD: %s kept %v, %s there %v", first, exists(t, prefix, "Cellar/tool/"+first), second, exists(t, prefix, "Cellar/tool/"+second))
	}
	if b, _ := os.ReadFile(filepath.Join(prefix, "bin/tool")); string(b) != "#!/bin/sh\necho two\n" {
		t.Errorf("bin/tool = %q", b)
	}

	third := "HEAD-" + newCommit(t, repo, "three")
	if out, err := runCmd(t, e, "reinstall", "-prefix", prefix, "tool"); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, stderr())
	}
	if exists(t, prefix, "Cellar/tool/"+second) || !exists(t, prefix, "Cellar/tool/"+third) {
		t.Errorf("after reinstall: %s kept %v, %s there %v", second, exists(t, prefix, "Cellar/tool/"+second), third, exists(t, prefix, "Cellar/tool/"+third))
	}
}

// brew will not install HEAD over a linked stable keg, nor for a formula
// without a head.
func TestInstallHEADRefusals(t *testing.T) {
	e, prefix, origin, reg := tapBottleStore(t)
	repo, _ := headRepo(t, "head")
	url, sum := reg.AddSource("tool-1.0.tar.gz", installtest.Tarball(t, map[string]string{"tool-1.0/tool": "#!/bin/sh\necho stable\n"}))
	writeTapFormula(t, origin, "tool", headFormula(url, sum, repo))
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "u/t/tool"); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	_, err := runCmd(t, e, "install", "-prefix", prefix, "--HEAD", "u/t/tool")
	if err == nil || !strings.Contains(e.err.(interface{ String() string }).String(),
		"tool 1.0 is already installed\nTo install HEAD, first run:\n  brw unlink tool\n") {
		t.Errorf("%v\n%s", err, e.err)
	}
	if exists(t, prefix, "Cellar/tool/HEAD") {
		t.Error("a HEAD keg was made")
	}

	writeTapFormula(t, origin, "nohead", "class Nohead < Formula\n  url \""+url+"\"\n  sha256 \""+sum+"\"\n  def install\n  end\nend\n")
	gitIn(t, tap.Dir(prefix, "u/t"), "pull", "-q")
	e.err.(interface{ Reset() }).Reset()
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "--HEAD", "u/t/nohead"); err == nil ||
		!strings.Contains(e.err.(interface{ String() string }).String(), "No head is defined for u/t/nohead") {
		t.Errorf("%v\n%s", err, e.err)
	}
}
