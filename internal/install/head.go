package install

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/outdated"
	"github.com/jigsmiths/brw/internal/rb"
	"github.com/jigsmiths/brw/internal/tap"
)

// buildSource is an archive or a gitCheckout.
type buildSource interface {
	sourceURL() string
	headVersion() string
}

// archive is a stable download, or a head fetched as brew's curl strategy does.
type archive struct{ url, path string }

func (a archive) sourceURL() string { return a.url }
func (archive) headVersion() string { return "HEAD" }

type gitCheckout struct {
	url, dir, short, revision string
	time                      time.Time
}

func (c gitCheckout) sourceURL() string   { return c.url }
func (c gitCheckout) headVersion() string { return "HEAD-" + c.short }

// headStrategy is brew's DownloadStrategyDetector for a head URL.
func headStrategy(h *rb.HeadSource) string {
	if h.Using != "" {
		return h.Using
	}
	for _, re := range gitURLs {
		if re.MatchString(h.URL) {
			return "git"
		}
	}
	for name, re := range otherVCS {
		if re.MatchString(h.URL) {
			return name
		}
	}
	return "curl"
}

var gitURLs = []*regexp.Regexp{
	regexp.MustCompile(`^https?://.+\.git$`), regexp.MustCompile(`^git://`),
	regexp.MustCompile(`^https?://git\.sr\.ht/[^/]+/[^/]+$`), regexp.MustCompile(`^https?://tangled\.sh/[^/]+/[^/]+$`),
	regexp.MustCompile(`^ssh://git`),
}

var otherVCS = map[string]*regexp.Regexp{
	"svn": regexp.MustCompile(`^(https?://svn\.|svn://|svn\+http://|http://svn\.apache\.org/repos/|https?://(?:[A-Za-z0-9\-.]+\.)?sourceforge\.net/svnroot/)`),
	"cvs": regexp.MustCompile(`^cvs://`),
	"hg":  regexp.MustCompile(`^(hg://|https?://(?:[A-Za-z0-9\-.]+\.)?sourceforge\.net/hgweb/)`),
	"bzr": regexp.MustCompile(`^bzr://`), "fossil": regexp.MustCompile(`^fossil://`),
}

func (in *Installer) fetchFormulaHead(ctx context.Context, f *api.Formula) (buildSource, error) {
	if in.LoadSource == nil {
		return nil, ErrNoSource
	}
	src, err := in.LoadSource(f)
	if err != nil {
		return nil, err
	}
	h, ok := src.Formula.Head()
	if !ok {
		return nil, fmt.Errorf("No head is defined for %s", f.FullName) //nolint:staticcheck // brew's message
	}
	in.logf("==> Cloning %s", h.URL)
	return in.fetchHead(ctx, in.cacheRoot(), f.Name, &h)
}

// cacheRoot holds the downloads directory, and the head clones beside it as in brew's cache.
func (in *Installer) cacheRoot() string { return filepath.Dir(in.Fetcher.Dir) }

type Heads struct {
	Cache     string
	GitHubAPI string
	HTTP      *http.Client
}

var githubHead = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)\.git$`)

func (hs Heads) Check(ctx context.Context, f *api.Formula, installed string, fetch bool) (outdated.HeadState, error) {
	h := &rb.HeadSource{URL: f.URLs.Head.URL, Branch: f.URLs.Head.Branch, Using: f.URLs.Head.Using}
	if h.URL == "" || headStrategy(h) != "git" {
		return outdated.HeadState{}, nil
	}
	if m := githubHead.FindStringSubmatch(h.URL); m != nil && h.Using == "" && os.Getenv("HOMEBREW_NO_GITHUB_API") == "" {
		_, ref := gitRef(h)
		if c := hs.githubCommit(ctx, m[1], m[2], ref); c != "" {
			newer := !strings.HasPrefix(c, installed) || hs.ambiguous(ctx, m[1], m[2], installed)
			return outdated.HeadState{Latest: c, Newer: fetch && newer}, nil
		}
	}
	if fetch {
		s, err := fetchGit(ctx, hs.Cache, f.Name, h)
		return outdated.HeadState{Latest: s.short, Newer: s.short != installed}, err
	}
	cmd := exec.CommandContext(ctx, "git", "--git-dir", filepath.Join(hs.Cache, f.Name+"--git", ".git"), "rev-parse", "--short=7", "HEAD")
	cmd.Env = tap.GitEnv()
	out, err := cmd.Output()
	if err != nil {
		return outdated.HeadState{}, nil // no clone: brew's last_commit is empty too
	}
	return outdated.HeadState{Latest: strings.TrimSpace(string(out))}, nil
}

// githubCommit is brew's GitHub.last_commit: GitHub's API puts the commit in a HEAD request's ETag.
func (hs Heads) githubCommit(ctx context.Context, user, repo, ref string) string {
	resp, err := hs.githubHEAD(ctx, user, repo, ref)
	if err != nil {
		return ""
	}
	m := etagSHA.FindStringSubmatch(resp.Header.Get("ETag"))
	if m == nil || len(m[1]) < 7 || hs.ambiguous(ctx, user, repo, m[1][:7]) {
		return ""
	}
	return m[1][:7]
}

var etagSHA = regexp.MustCompile(`^"([0-9a-fA-F]+)"$`)

// ambiguous is brew's multiple_short_commits_exist?: true unless GitHub finds
// exactly one commit for the short one.
func (hs Heads) ambiguous(ctx context.Context, user, repo, commit string) bool {
	resp, err := hs.githubHEAD(ctx, user, repo, commit)
	return err != nil || resp.StatusCode != http.StatusOK
}

func (hs Heads) githubHEAD(ctx context.Context, user, repo, ref string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		hs.GitHubAPI+"/repos/"+url.PathEscape(user)+"/"+url.PathEscape(repo)+"/commits/"+url.PathEscape(ref), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.sha")
	if token := os.Getenv("HOMEBREW_GITHUB_API_TOKEN"); token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	client := hs.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	return resp, nil
}

func (in *Installer) fetchHead(ctx context.Context, cache, name string, h *rb.HeadSource) (buildSource, error) {
	switch s := headStrategy(h); s {
	case "git":
		return fetchGit(ctx, cache, name, h)
	case "curl", "nounzip", "homebrew_curl":
		p, err := in.Fetcher.FetchUnverified(ctx, name, "HEAD", h.URL)
		return archive{url: h.URL, path: p}, err
	default:
		return nil, fmt.Errorf("brw cannot fetch a head from %s with the %s strategy yet", h.URL, s)
	}
}

// copyCheckout stages a checkout as brew's Directory strategy does: cp -pR
// of everything in it, .git included.
func copyCheckout(dir, stage string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	args := []string{"-pR"}
	for _, e := range entries {
		args = append(args, filepath.Join(dir, e.Name()))
	}
	if out, err := exec.Command("/bin/cp", append(args, stage)...).CombinedOutput(); err != nil {
		return fmt.Errorf("staging %s: %w: %s", dir, err, out)
	}
	return nil
}

func gitRef(h *rb.HeadSource) (kind, ref string) {
	switch {
	case h.Tag != "":
		return "tag", h.Tag
	case h.Branch != "":
		return "branch", h.Branch
	case h.Revision != "":
		return "revision", h.Revision
	}
	return "branch", "master"
}

func fetchGit(ctx context.Context, cache, name string, h *rb.HeadSource) (gitCheckout, error) {
	kind, ref := gitRef(h)
	onlyPath := h.OnlyPath
	if onlyPath != "" {
		onlyPath = "/" + strings.Trim(onlyPath, "/") + "/"
	}
	tag := "git"
	if onlyPath != "" {
		tag = "git-sparse"
	}
	dir := filepath.Join(cache, name+"--"+tag)
	git := func(chdir bool, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		if chdir {
			cmd.Dir = dir
		}
		cmd.Env = append(tap.GitEnv(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out)), nil
	}
	sparse := func() error {
		for _, kv := range [][2]string{{"core.sparseCheckout", "true"}, {"core.sparseCheckoutCone", "true"}} {
			if _, err := git(true, "config", kv[0], kv[1]); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Join(dir, ".git", "info"), 0o755); err != nil { //fsx:allow the download cache
			return err
		}
		return os.WriteFile(filepath.Join(dir, ".git", "info", "sparse-checkout"), []byte(onlyPath+"\n"), 0o644) //fsx:allow the download cache
	}
	submodules := func() error {
		if _, err := os.Stat(filepath.Join(dir, ".gitmodules")); err != nil {
			return nil
		}
		if _, err := git(true, "submodule", "foreach", "--recursive", "git submodule sync"); err != nil {
			return err
		}
		_, err := git(true, "submodule", "update", "--init", "--recursive")
		return err
	}
	checkout := func() error {
		_, err := git(true, "checkout", "--no-recurse-submodules", "-f", ref, "--")
		return err
	}
	clone := func() error {
		if err := os.MkdirAll(cache, 0o755); err != nil { //fsx:allow the download cache
			return err
		}
		args := []string{"clone", "--no-recurse-submodules"}
		if kind == "branch" || kind == "tag" {
			args = append(args, "--branch", ref)
		}
		if onlyPath != "" {
			args = append(args, "--no-checkout", "--filter=blob:none")
		}
		args = append(args, "--config", "advice.detachedHead=false", "--config", "core.fsmonitor=false", "--end-of-options", h.URL, dir)
		if _, err := git(false, args...); err != nil {
			return err
		}
		if _, err := git(true, "config", "homebrew.cacheversion", "0"); err != nil {
			return err
		}
		if onlyPath != "" {
			if err := sparse(); err != nil {
				return err
			}
		}
		if err := checkout(); err != nil {
			return err
		}
		return submodules()
	}
	update := func() error {
		refspec := "+refs/heads/*:refs/remotes/origin/*"
		switch kind {
		case "branch":
			refspec = "+refs/heads/" + ref + ":refs/remotes/origin/" + ref
		case "tag":
			refspec = "+refs/tags/" + ref + ":refs/tags/" + ref
		}
		for _, kv := range [][2]string{{"remote.origin.url", h.URL}, {"remote.origin.fetch", refspec},
			{"remote.origin.tagOpt", "--no-tags"}, {"advice.detachedHead", "false"}, {"core.fsmonitor", "false"}} {
			if _, err := git(true, "config", kv[0], kv[1]); err != nil {
				return err
			}
		}
		if onlyPath != "" {
			if _, err := git(true, "config", "origin.partialclonefilter", "blob:none"); err != nil {
				return err
			}
			if err := sparse(); err != nil {
				return err
			}
		}
		_, haveRef := git(true, "rev-parse", "-q", "--verify", "--end-of-options", ref+"^{commit}")
		if kind == "branch" || haveRef != nil {
			args := []string{"fetch", "--no-recurse-submodules", "origin"}
			if _, err := os.Stat(filepath.Join(dir, ".git", "shallow")); err == nil {
				args = append(args, "--unshallow")
			}
			if _, err := git(true, args...); err != nil {
				return err
			}
		}
		if err := checkout(); err != nil {
			return err
		}
		reset := []string{"reset", "--no-recurse-submodules", "--hard"}
		switch kind {
		case "branch":
			reset = append(reset, "origin/"+ref)
		case "tag", "revision":
			reset = append(reset, ref)
		}
		if _, err := git(true, append(reset, "--")...); err != nil {
			return err
		}
		return submodules()
	}

	var err error
	switch _, statErr := os.Stat(dir); {
	case statErr != nil:
		err = clone()
	case func() bool { _, e := git(false, "-C", dir, "status", "-s"); return e == nil }():
		err = update()
	default:
		if err = os.RemoveAll(dir); err == nil { //fsx:allow an invalid repository in the download cache
			err = clone()
		}
	}
	if err != nil {
		return gitCheckout{}, err
	}
	gitDir := filepath.Join(dir, ".git")
	short, err := git(false, "--git-dir", gitDir, "rev-parse", "--short=7", "HEAD")
	if err != nil {
		return gitCheckout{}, err
	}
	full, err := git(false, "--git-dir", gitDir, "rev-parse", "-q", "--verify", "HEAD")
	if err != nil {
		return gitCheckout{}, err
	}
	when, err := git(false, "--git-dir", gitDir, "show", "-s", "--format=%ct")
	if err != nil {
		return gitCheckout{}, err
	}
	secs, err := strconv.ParseInt(when, 10, 64)
	if err != nil {
		return gitCheckout{}, fmt.Errorf("the commit time %q: %w", when, err)
	}
	if kind == "tag" && h.Revision != "" && full != h.Revision {
		return gitCheckout{}, errors.New(ref + " tag should be " + h.Revision + "\nbut is actually " + full)
	}
	return gitCheckout{url: h.URL, dir: dir, short: short, revision: full, time: time.Unix(secs, 0)}, nil
}
