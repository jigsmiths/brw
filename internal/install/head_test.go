package install

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
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

// fakeGitHub answers HEAD /repos/u/tool/commits/<ref> as GitHub's API does,
// with the commit in the ETag, for the refs it knows.
func fakeGitHub(t *testing.T, refs map[string]string) (*httptest.Server, *[]string) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.Path+" "+r.Header.Get("Accept")+" "+r.Header.Get("Authorization"))
		sha, ok := refs[strings.TrimPrefix(r.URL.Path, "/repos/u/tool/commits/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", `"`+sha+`"`)
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

func githubTool(h map[string]any) *api.Formula {
	f := &api.Formula{Name: "tool"}
	f.URLs.Head.URL, _ = h["url"].(string)
	f.URLs.Head.Branch, _ = h["branch"].(string)
	f.URLs.Head.Using, _ = h["using"].(string)
	return f
}

// brew's GitHubGitDownloadStrategy asks GitHub's API for the branch's
// commit and fetches nothing when that answers.
func TestHeadsAskGitHubFirst(t *testing.T) {
	full := "a82d0d6" + strings.Repeat("1", 33)
	srv, asked := fakeGitHub(t, map[string]string{"main": full, "a82d0d6": full, "cf6a8f9": "cf6a8f9" + strings.Repeat("2", 33)})
	t.Setenv("HOMEBREW_GITHUB_API_TOKEN", "secret")
	t.Setenv("HOMEBREW_NO_GITHUB_API", "")
	hs := Heads{Cache: t.TempDir(), API: srv.URL, HTTP: srv.Client()}
	f := githubTool(map[string]any{"url": "https://github.com/u/tool.git", "branch": "main"})

	latest, newer, vcs, err := hs.Check(t.Context(), f, "cf6a8f9", true)
	if err != nil || latest != "a82d0d6" || !newer || !vcs {
		t.Errorf("a newer commit: %q, %v, %v, %v", latest, newer, vcs, err)
	}
	if want := "HEAD /repos/u/tool/commits/main application/vnd.github.sha token secret"; len(*asked) == 0 || (*asked)[0] != want {
		t.Errorf("asked %q, want first %q", *asked, want)
	}
	if latest, newer, _, err := hs.Check(t.Context(), f, "a82d0d6", true); err != nil || latest != "a82d0d6" || newer {
		t.Errorf("the installed commit: %q, %v, %v", latest, newer, err)
	}
	if _, err := os.Stat(filepath.Join(hs.Cache, "tool--git")); err == nil {
		t.Error("the repository was cloned although the API answered")
	}

	// Not GitHub's to answer: using: :git, or HOMEBREW_NO_GITHUB_API.
	*asked = nil
	repo := t.TempDir()
	gitT(t, repo, "init", "-q", "-b", "main")
	commit := commitFile(t, repo, "a", "1")
	local := githubTool(map[string]any{"url": "file://" + repo, "branch": "main", "using": "git"})
	if latest, newer, _, err := hs.Check(t.Context(), local, "0000000", true); err != nil || latest != commit[:7] || !newer || len(*asked) != 0 {
		t.Errorf("a repository off GitHub: %q, %v, %v; asked %q", latest, newer, err, *asked)
	}
	t.Setenv("HOMEBREW_NO_GITHUB_API", "1")
	if _, _, _, _ = hs.Check(t.Context(), f, "cf6a8f9", false); len(*asked) != 0 {
		t.Errorf("HOMEBREW_NO_GITHUB_API still asked %q", *asked)
	}
}

// An API answer brew cannot use, such as an unknown ref, falls back to git.
func TestHeadsFallBackToGitWhenGitHubCannotSay(t *testing.T) {
	srv, _ := fakeGitHub(t, map[string]string{})
	t.Setenv("HOMEBREW_NO_GITHUB_API", "")
	hs := Heads{Cache: t.TempDir(), API: srv.URL, HTTP: srv.Client()}
	f := githubTool(map[string]any{"url": "https://github.com/u/tool.git"})
	if latest, newer, vcs, err := hs.Check(t.Context(), f, "cf6a8f9", false); err != nil || latest != "" || newer || !vcs {
		t.Errorf("no clone and no answer: %q, %v, %v, %v", latest, newer, vcs, err)
	}
}
