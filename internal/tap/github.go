package tap

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const GitHubAPI = "https://api.github.com"

// Unchanged says whether the tap's branch on GitHub is still at the tap's
// HEAD, so update can skip the fetch as brew update does. It says false
// whenever it cannot tell, such as for a private tap, so the tap is pulled.
func Unchanged(ctx context.Context, client *http.Client, api string, t Tap) bool {
	repo, ok := githubRepo(t.Remote())
	if !ok {
		return false
	}
	upstream, err := run(ctx, t.Dir, "rev-parse", "--abbrev-ref", "@{upstream}")
	if err != nil {
		return false
	}
	branch, ok := strings.CutPrefix(upstream, "origin/")
	if !ok {
		return false
	}
	head, err := run(ctx, t.Dir, "rev-parse", "HEAD")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/commits/%s", api, repo, branch), nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/vnd.github.sha")
	req.Header.Set("If-None-Match", `"`+head+`"`)
	if token := os.Getenv("HOMEBREW_GITHUB_API_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return true
	case http.StatusOK:
		sha, err := io.ReadAll(io.LimitReader(resp.Body, 128))
		return err == nil && strings.EqualFold(strings.TrimSpace(string(sha)), head)
	}
	return false
}

func githubRepo(remote string) (string, bool) {
	for _, p := range []string{"https://github.com/", "git@github.com:", "ssh://git@github.com/"} {
		if rest, ok := strings.CutPrefix(remote, p); ok {
			repo := strings.TrimSuffix(rest, ".git")
			if owner, name, ok := strings.Cut(repo, "/"); ok && owner != "" && name != "" && !strings.Contains(name, "/") {
				return repo, true
			}
		}
	}
	return "", false
}
