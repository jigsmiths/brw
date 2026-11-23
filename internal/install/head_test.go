package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/rb"
	"github.com/jigsmiths/brw/internal/tap"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = tap.GitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, repo, name, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-q", "-m", name)
	return gitT(t, repo, "rev-parse", "HEAD")
}

// A second fetch brings the cached clone up to date as brew's update does:
// with the branch's refspec, fetched and reset to it.
func TestFetchGitUpdatesItsClone(t *testing.T) {
	repo, cache := t.TempDir(), t.TempDir()
	gitT(t, repo, "init", "-q", "-b", "main")
	first := commitFile(t, repo, "a", "1")
	h := &rb.HeadSource{URL: "file://" + repo, Using: "git", Branch: "main"}
	got, err := fetchGit(t.Context(), cache, "tool", h)
	if err != nil {
		t.Fatal(err)
	}
	if got.revision != first || got.short != first[:7] || got.version() != "HEAD-"+first[:7] || got.dir != filepath.Join(cache, "tool--git") {
		t.Errorf("first fetch = %+v", got)
	}
	second := commitFile(t, repo, "a", "2")
	if got, err = fetchGit(t.Context(), cache, "tool", h); err != nil || got.revision != second {
		t.Fatalf("second fetch = %+v, %v; want %s", got, err, second)
	}
	if b, _ := os.ReadFile(filepath.Join(got.dir, "a")); string(b) != "2" {
		t.Errorf("checkout has a = %q", b)
	}
	if spec := gitT(t, got.dir, "config", "remote.origin.fetch"); spec != "+refs/heads/main:refs/remotes/origin/main" {
		t.Errorf("refspec = %s", spec)
	}
}

func TestFetchGitDefaultsToMasterAndChecksATagsRevision(t *testing.T) {
	repo := t.TempDir()
	gitT(t, repo, "init", "-q", "-b", "master")
	commit := commitFile(t, repo, "a", "1")
	gitT(t, repo, "tag", "v1")
	if got, err := fetchGit(t.Context(), t.TempDir(), "tool", &rb.HeadSource{URL: "file://" + repo, Using: "git"}); err != nil || got.revision != commit {
		t.Errorf("no ref: %+v, %v", got, err)
	}
	h := &rb.HeadSource{URL: "file://" + repo, Using: "git", Tag: "v1", Revision: strings.Repeat("0", 40)}
	if _, err := fetchGit(t.Context(), t.TempDir(), "tool", h); err == nil || !strings.Contains(err.Error(), "v1 tag should be "+h.Revision+"\nbut is actually "+commit) {
		t.Errorf("wrong tag revision: %v", err)
	}
}

func TestHeadStrategyIsBrews(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/acsandmann/rift.git": "git",
		"git://example.com/x":                    "git",
		"https://git.sr.ht/~user/repo":           "git",
		"https://github.com/eksctl-io/eksctl/releases/download/latest_release/eksctl.tar.gz": "curl",
		"https://svn.example.com/trunk": "svn",
		"hg://example.com/x":            "hg",
	} {
		if got := headStrategy(&rb.HeadSource{URL: url}); got != want {
			t.Errorf("%s: %s, want %s", url, got, want)
		}
	}
	if got := headStrategy(&rb.HeadSource{URL: "https://example.com/x", Using: "git"}); got != "git" {
		t.Errorf("using: :git = %s", got)
	}
}
