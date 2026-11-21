// Package tap reads and updates the taps of a Homebrew prefix. brw cannot run a
// tap's Ruby, so it uses the casks a tap lists in api/cask.json, in the API's
// format, as brew info --json=v2 --variations writes it.
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
)

// Tap is a tapped repository.
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

// Casks are the casks the tap lists whose cask file is as it was when the
// listing was generated. A tap without a listing has none.
func (t Tap) Casks() ([]api.Cask, error) {
	b, err := os.ReadFile(filepath.Join(t.Dir, "api", "cask.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	listed, err := api.ParseCasks(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.Name, err)
	}
	// brew records the tap's HEAD; without git, the listing's commit stands in.
	head, _ := run(context.Background(), t.Dir, "rev-parse", "HEAD")
	var out []api.Cask
	for _, c := range listed {
		if head != "" {
			c.TapGitHead = head
		}
		file := filepath.Join(t.Dir, filepath.Clean("/"+c.RubySourcePath))
		rb, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		if sum := sha256.Sum256(rb); hex.EncodeToString(sum[:]) != c.RubySourceChecksum.SHA256 {
			continue
		}
		c.Tap, c.SourceFile = t.Name, file
		out = append(out, c)
	}
	return out, nil
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
