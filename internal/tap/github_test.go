package tap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// githubTap is a clone of a fresh origin that says it came from GitHub.
func githubTap(t *testing.T, remote string) (Tap, string) {
	t.Helper()
	tp, err := Clone(context.Background(), t.TempDir(), "jigsmiths/tap", origin(t))
	if err != nil {
		t.Fatal(err)
	}
	git(t, tp.Dir, "remote", "set-url", "origin", remote)
	return tp, strings.TrimSpace(git(t, tp.Dir, "rev-parse", "HEAD"))
}

// github answers as GitHub's API does for a repository whose branch is at sha.
func github(t *testing.T, sha string, asked *atomic.Int32) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if r.URL.Path != "/repos/jigsmiths/homebrew-tap/commits/main" || r.Header.Get("Accept") != "application/vnd.github.sha" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("If-None-Match") == `"`+sha+`"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(sha))
	}))
	t.Cleanup(s.Close)
	return s
}

func unchanged(api string, tp Tap) bool {
	same, _ := Unchanged(context.Background(), http.DefaultClient, api, tp)
	return same
}

func TestUnchangedAsksGitHubForTheBranchHead(t *testing.T) {
	for _, remote := range []string{
		"https://github.com/jigsmiths/homebrew-tap",
		"https://github.com/jigsmiths/homebrew-tap.git",
		"git@github.com:jigsmiths/homebrew-tap.git",
		"ssh://git@github.com/jigsmiths/homebrew-tap",
	} {
		tp, head := githubTap(t, remote)
		var asked atomic.Int32
		if !unchanged(github(t, head, &asked).URL, tp) {
			t.Errorf("%s at GitHub's head: Unchanged = false", remote)
		}
		if !unchanged(github(t, strings.ToUpper(head), &asked).URL, tp) {
			t.Errorf("%s with GitHub's head in capitals: Unchanged = false", remote)
		}
		if unchanged(github(t, "0123abcd", &asked).URL, tp) {
			t.Errorf("%s behind GitHub: Unchanged = true", remote)
		}
	}
}

func TestUnchangedSendsTheGitHubToken(t *testing.T) {
	t.Setenv("HOMEBREW_GITHUB_API_TOKEN", "secret")
	tp, _ := githubTap(t, "https://github.com/jigsmiths/homebrew-tap")
	var got string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer s.Close()
	unchanged(s.URL, tp)
	if got != "Bearer secret" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestUnchangedAsksWithoutARejectedToken(t *testing.T) {
	t.Setenv("HOMEBREW_GITHUB_API_TOKEN", "expired")
	tp, _ := githubTap(t, "https://github.com/jigsmiths/homebrew-tap")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer s.Close()
	same, tokenRejected := Unchanged(context.Background(), http.DefaultClient, s.URL, tp)
	if !same || !tokenRejected {
		t.Errorf("Unchanged = %v, %v; want true, true", same, tokenRejected)
	}
}

// When it cannot tell, the tap is pulled.
func TestUnchangedIsFalseWhenGitHubCannotSay(t *testing.T) {
	tp, _ := githubTap(t, "https://github.com/jigsmiths/homebrew-tap")
	for name, status := range map[string]int{"private": http.StatusNotFound, "rate limited": http.StatusForbidden} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		if unchanged(s.URL, tp) {
			t.Errorf("%s: Unchanged = true", name)
		}
		s.Close()
	}
	if unchanged("http://127.0.0.1:1", tp) {
		t.Error("offline: Unchanged = true")
	}

	var asked atomic.Int32
	elsewhere, head := githubTap(t, "https://gitlab.com/jigsmiths/homebrew-tap")
	if unchanged(github(t, head, &asked).URL, elsewhere) || asked.Load() != 0 {
		t.Errorf("a tap not on GitHub: asked GitHub %d times", asked.Load())
	}
	if unchanged("http://127.0.0.1:1", Tap{Name: "u/t", Dir: t.TempDir()}) {
		t.Error("a folder that is no clone: Unchanged = true")
	}
}
