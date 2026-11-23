// Package tap reads and updates the taps of a Homebrew prefix. A tap's casks
// are read from its cask files, as brew reads them.
package tap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/caskrb"
)

type Tap struct {
	Name string // user/repo
	Dir  string
}

// Dir is where brew keeps the tap called name.
func Dir(prefix, name string) string {
	user, repo, _ := strings.Cut(name, "/")
	return filepath.Join(prefix, "Library", "Taps", user, "homebrew-"+repo)
}

// Split splits a full name such as jigsmiths/tap/brw into its tap and token.
func Split(name string) (tap, token string, ok bool) {
	parts := strings.Split(name, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return strings.ToLower(parts[0] + "/" + parts[1]), parts[2], true
}

// List finds the taps of a prefix, sorted by name.
func List(prefix string) ([]Tap, error) {
	root := filepath.Join(prefix, "Library", "Taps")
	users, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Tap
	for _, u := range users {
		if !u.IsDir() || strings.HasPrefix(u.Name(), ".") {
			continue
		}
		repos, err := os.ReadDir(filepath.Join(root, u.Name()))
		if err != nil {
			return nil, err
		}
		for _, r := range repos {
			repo, ok := strings.CutPrefix(r.Name(), "homebrew-")
			if !ok || !r.IsDir() {
				continue
			}
			out = append(out, Tap{Name: u.Name() + "/" + repo, Dir: filepath.Join(root, u.Name(), r.Name())})
		}
	}
	return out, nil
}

// Casks are the tap's casks as brew loads them on the platform tag, such as
// "arm64_tahoe". A cask file brw cannot evaluate is left out; Cask says why.
func (t Tap) Casks(tag string) ([]api.Cask, error) {
	files, err := t.caskFiles()
	if err != nil {
		return nil, err
	}
	head, _ := run(context.Background(), t.Dir, "rev-parse", "HEAD")
	var out []api.Cask
	for _, f := range files {
		c, err := t.load(f, tag, head)
		if err != nil {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// Cask loads the tap's cask called token, or says why brw cannot.
func (t Tap) Cask(token, tag string) (api.Cask, error) {
	files, err := t.caskFiles()
	if err != nil {
		return api.Cask{}, err
	}
	for _, f := range files {
		if strings.EqualFold(strings.TrimSuffix(filepath.Base(f), ".rb"), token) {
			head, _ := run(context.Background(), t.Dir, "rev-parse", "HEAD")
			return t.load(f, tag, head)
		}
	}
	return api.Cask{}, fmt.Errorf("%s has no cask %s: %w", t.Name, token, fs.ErrNotExist)
}

// caskFiles are the files under the tap's Casks folder, where brew looks.
func (t Tap) caskFiles() ([]string, error) {
	var files []string
	err := filepath.WalkDir(filepath.Join(t.Dir, "Casks"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == filepath.Join(t.Dir, "Casks") && errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipDir
			}
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".rb") {
			files = append(files, p)
		}
		return nil
	})
	return files, err
}

// load evaluates a cask file and records where it came from, as brew's
// receipt does: the tap's HEAD, the file and its checksum.
func (t Tap) load(file, tag, head string) (api.Cask, error) {
	rb, err := os.ReadFile(file)
	if err != nil {
		return api.Cask{}, err
	}
	c, err := caskrb.Load(rb, tag)
	if err != nil {
		return api.Cask{}, fmt.Errorf("%s/%s: %w", t.Name, strings.TrimSuffix(filepath.Base(file), ".rb"), err)
	}
	rel, _ := filepath.Rel(t.Dir, file)
	sum := sha256.Sum256(rb)
	c.Tap, c.FullToken, c.TapGitHead, c.SourceFile = t.Name, t.Name+"/"+c.Token, head, file
	c.RubySourcePath, c.RubySourceChecksum.SHA256 = filepath.ToSlash(rel), hex.EncodeToString(sum[:])
	return c, nil
}

// Remote is where the tap was cloned from, or "" when it cannot be read.
func (t Tap) Remote() string {
	url, err := run(context.Background(), t.Dir, "config", "--local", "--get", "remote.origin.url")
	if err != nil {
		return ""
	}
	return url
}

// Clone taps name from remote, as brew tap does. git writes the tap, so its
// writes do not go through fsx.
func Clone(ctx context.Context, prefix, name, remote string) (Tap, error) {
	t := Tap{Name: name, Dir: Dir(prefix, name)}
	if _, err := os.Stat(t.Dir); err == nil {
		return t, fmt.Errorf("%s is already tapped", name)
	}
	_, err := run(ctx, "", "clone", "--origin=origin", "-q", "--template=", "--config", "core.fsmonitor=false", "--end-of-options", remote, t.Dir)
	return t, err
}

// Pull fast-forwards the tap to its remote and says whether it moved.
func Pull(ctx context.Context, t Tap) (changed bool, err error) {
	before, err := run(ctx, t.Dir, "rev-parse", "HEAD")
	if err != nil {
		return false, err
	}
	if _, err := run(ctx, t.Dir, "pull", "--ff-only", "--no-rebase", "-q"); err != nil {
		return false, err
	}
	after, err := run(ctx, t.Dir, "rev-parse", "HEAD")
	return before != after, err
}

// repoVars point git at a repository other than the one it runs in, as inside
// a git hook.
var repoVars = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_PREFIX"}

// GitEnv is the environment without repoVars, so git works on the tap.
func GitEnv() []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(repoVars, name)
	})
}

// run runs git without the tap's hooks or a password prompt, as brew does.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	pre := []string{"-c", "core.hooksPath=" + os.DevNull}
	if dir != "" {
		pre = append(pre, "-C", dir)
	}
	cmd := exec.CommandContext(ctx, "git", append(pre, args...)...)
	cmd.Env = append(GitEnv(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}
