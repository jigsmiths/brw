package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/rb"
	"github.com/jigsmiths/brw/internal/tap"
)

// headSource is a formula's head download, fetched: a git checkout in the
// cache, or an archive brew's curl strategy downloaded.
type headSource struct {
	url      string
	dir      string // the checkout; empty for an archive
	archive  string
	short    string // the commit as brew abbreviates it, which names the keg
	revision string // the full commit, which the receipt records
	time     time.Time
}

// version is the head spec's version once fetched: HEAD-<commit> for a
// repository, HEAD for an archive.
func (h headSource) version() string {
	if h.short == "" {
		return "HEAD"
	}
	return "HEAD-" + h.short
}

// headStrategy is brew's DownloadStrategyDetector for a head URL: "git",
// "curl", or the name of a strategy brw cannot use.
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

// fetchFormulaHead fetches f's head download into the cache, beside the
// downloads, where brew keeps its clones.
func (in *Installer) fetchFormulaHead(ctx context.Context, f *api.Formula) (headSource, error) {
	if in.LoadSource == nil {
		return headSource{}, ErrNoSource
	}
	src, err := in.LoadSource(f)
	if err != nil {
		return headSource{}, err
	}
	h := src.Formula.Head()
	if h == nil {
		return headSource{}, fmt.Errorf("No head is defined for %s", f.FullName) //nolint:staticcheck // brew's message
	}
	in.logf("==> Cloning %s", h.URL)
	return in.fetchHead(ctx, filepath.Dir(in.Fetcher.Dir), f.Name, h)
}

// fetchHead fetches a formula's head download into cache, as brew's fetch
// does for install --HEAD.
func (in *Installer) fetchHead(ctx context.Context, cache, name string, h *rb.HeadSource) (headSource, error) {
	switch s := headStrategy(h); s {
	case "git":
		return fetchGit(ctx, cache, name, h)
	case "curl", "nounzip", "homebrew_curl":
		archive, err := in.Fetcher.FetchUnverified(ctx, name, "HEAD", h.URL)
		return headSource{url: h.URL, archive: archive}, err
	default:
		return headSource{}, fmt.Errorf("brw cannot fetch a head from %s with the %s strategy yet", h.URL, s)
	}
}

// copyCheckout stages a checkout as brew's Directory strategy does: cp -pR
// of everything in it, .git included, which cp writes outside fsx as tar does.
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

// gitRef is the ref brew's GitDownloadStrategy checks out: the branch, tag or
// revision the url names, else the branch master.
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

// fetchGit is brew's GitDownloadStrategy#fetch: clone into the cache, or
// bring the clone there up to date, and check out the ref.
func fetchGit(ctx context.Context, cache, name string, h *rb.HeadSource) (headSource, error) {
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
		return headSource{}, err
	}
	gitDir := filepath.Join(dir, ".git")
	short, err := git(false, "--git-dir", gitDir, "rev-parse", "--short=7", "HEAD")
	if err != nil {
		return headSource{}, err
	}
	full, err := git(false, "--git-dir", gitDir, "rev-parse", "-q", "--verify", "HEAD")
	if err != nil {
		return headSource{}, err
	}
	when, err := git(false, "--git-dir", gitDir, "show", "-s", "--format=%ct")
	if err != nil {
		return headSource{}, err
	}
	secs, err := strconv.ParseInt(when, 10, 64)
	if err != nil {
		return headSource{}, fmt.Errorf("the commit time %q: %w", when, err)
	}
	if kind == "tag" && h.Revision != "" && full != h.Revision {
		return headSource{}, errors.New(ref + " tag should be " + h.Revision + "\nbut is actually " + full)
	}
	return headSource{url: h.URL, dir: dir, short: short, revision: full, time: time.Unix(secs, 0)}, nil
}
