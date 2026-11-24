package tap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/trust"
)

const (
	CoreName = "homebrew/core"
	CaskName = "homebrew/cask"

	dirPerm       = 0o755
	cachePerm     = 0o644
	fetchRefspec  = "+refs/heads/*:refs/remotes/origin/*"
	defaultOwner  = "you"
	allowedTaps   = "HOMEBREW_ALLOWED_TAPS"
	forbiddenTaps = "HOMEBREW_FORBIDDEN_TAPS"
)

var (
	officialRepoPrefix = regexp.MustCompile(`^(?:home|linux)brew-`)
	remoteLike         = regexp.MustCompile(`^[^/]+:.`)
)

// deprecatedOfficial are the official taps brew has emptied.
var deprecatedOfficial = []string{
	"aliases", "apache", "binary", "bundle", "cask-drivers", "cask-eid", "cask-fonts", "cask-versions",
	"command-not-found", "completions", "devel-only", "dupes", "emacs", "formula-analytics", "fuse", "games",
	"gui", "head-only", "linux-fonts", "livecheck", "nginx", "php", "portable-ruby", "python", "science",
	"services", "test-bot", "tex", "versions", "x11",
}

// ParseName reads a tap name as brew does: user/repo, with a leading homebrew- or linuxbrew- dropped from the repo.
func ParseName(arg string) (string, error) {
	user, repo, _ := strings.Cut(arg, "/")
	invalid := func(part string) bool {
		return part == "" || part == "." || part == ".." || strings.Contains(part, "/")
	}
	if invalid(user) || invalid(repo) {
		return "", fmt.Errorf("Invalid tap name: '%s'", arg) //nolint:staticcheck // brew's wording
	}
	user = strings.ToLower(user)
	repo = strings.ToLower(officialRepoPrefix.ReplaceAllString(repo, ""))
	if (user == "homebrew" || user == "linuxbrew") && (repo == "core" || repo == "homebrew") {
		return CoreName, nil
	}
	return user + "/" + repo, nil
}

// DefaultRemote is where brew clones the tap from.
func DefaultRemote(name string) string {
	user, repo, _ := strings.Cut(name, "/")
	if user == "homebrew" || user == "linuxbrew" {
		user = strings.ToUpper(user[:1]) + user[1:]
	}
	return "https://github.com/" + user + "/homebrew-" + repo
}

func official(name string) bool { return strings.HasPrefix(name, "homebrew/") }

// CheckClonable refuses the taps brew says are gone or moved.
func CheckClonable(name string) error {
	user, repo, _ := strings.Cut(name, "/")
	switch {
	case official(name) && slices.Contains(deprecatedOfficial, repo):
		return fmt.Errorf("%s was deprecated. This tap is now empty and all its contents were either deleted or migrated.", name) //nolint:staticcheck // brew's wording
	case user == "caskroom" || name == "phinze/cask":
		moved := "cask"
		if repo != "cask" {
			moved = "cask-" + repo
		}
		return fmt.Errorf("%s was moved. Tap homebrew/%s instead.", name, moved) //nolint:staticcheck // brew's wording
	}
	return nil
}

// CheckEnv applies HOMEBREW_ALLOWED_TAPS and HOMEBREW_FORBIDDEN_TAPS to a tap and its remote, warning on stderr about invalid entries.
func CheckEnv(name, remote string, warn io.Writer) error {
	allowed, forbidden := references(allowedTaps, warn), references(forbiddenTaps, warn)
	implicit := official(name) && (remote == "" || trust.SameRemote(remote, DefaultRemote(name)))
	isAllowed := implicit || len(allowed) == 0 || slices.ContainsFunc(allowed, func(r string) bool { return matches(name, r, remote) })
	isForbidden := slices.ContainsFunc(forbidden, func(r string) bool { return matches(name, r, remote) })
	if isAllowed && !isForbidden {
		return nil
	}
	owner := os.Getenv("HOMEBREW_FORBIDDEN_OWNER")
	if owner == "" {
		owner = defaultOwner
	}
	user, repo, _ := strings.Cut(DefaultRemote(name)[len("https://github.com/"):], "/")
	msg := fmt.Sprintf("The installation of the %s/%s was requested but %s\n", user, repo, owner)
	if !isAllowed {
		msg += "has not allowed this tap in `$" + allowedTaps + "`"
	}
	if !isAllowed && isForbidden {
		msg += " and\n"
	}
	if isForbidden {
		msg += "has forbidden this tap in `$" + forbiddenTaps + "`"
	}
	msg += "."
	if contact := os.Getenv("HOMEBREW_FORBIDDEN_OWNER_CONTACT"); contact != "" {
		msg += "\n" + contact
	}
	return errors.New(msg)
}

func remoteReference(ref string) bool {
	return remoteLike.MatchString(ref) || strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, ".") || strings.HasPrefix(ref, "~")
}

func references(env string, warn io.Writer) []string {
	var out []string
	for ref := range strings.FieldsSeq(os.Getenv(env)) {
		if remoteReference(ref) {
			out = append(out, ref)
			continue
		}
		name, err := ParseName(ref)
		if err != nil {
			_, _ = fmt.Fprintf(warn, "brw: warning: Invalid tap name in `$%s`: %s\n", env, ref)
			continue
		}
		out = append(out, name)
	}
	return out
}

func matches(name, ref, remote string) bool {
	if remoteReference(ref) {
		return remote != "" && trust.SameRemote(ref, remote)
	}
	custom := remote != "" && !trust.SameRemote(remote, DefaultRemote(name))
	return !custom && name == strings.ToLower(ref)
}

// CheckGit says plainly when git is missing, since tapping runs it.
func CheckGit() error {
	if _, err := exec.LookPath("git"); err != nil {
		return errors.New("git is required to tap, and unlike brew, brw does not install it")
	}
	return nil
}

func Shallow(ctx context.Context, t Tap) bool {
	out, err := run(ctx, t.Dir, "rev-parse", "--is-shallow-repository")
	return err == nil && out == "true"
}

// Fetch updates a tap that is already cloned, making a shallow one whole.
func Fetch(ctx context.Context, t Tap) error {
	_, _ = run(ctx, t.Dir, "config", "--local", "--unset", "homebrew.forceautoupdate")
	args := []string{"fetch"}
	if Shallow(ctx, t) {
		args = append(args, "--unshallow")
	}
	_, err := run(ctx, t.Dir, args...)
	return err
}

// FixRemote points origin at requested and refetches, following the tap's default branch if it was renamed, as brew's fix_remote_configuration does.
func FixRemote(ctx context.Context, t Tap, requested string, log io.Writer) error {
	previous := t.Remote()
	if _, err := run(ctx, t.Dir, "remote", "set-url", "origin", "--end-of-options", requested); err != nil {
		return err
	}
	if _, err := run(ctx, t.Dir, "config", "remote.origin.fetch", fetchRefspec); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(log, "==> %s: changed remote from %s to %s\n", t.Name, previous, requested)
	current := originBranch(ctx, t)
	if _, err := run(ctx, t.Dir, "fetch", "origin", fetchRefspec); err != nil {
		return err
	}
	if _, err := run(ctx, t.Dir, "remote", "set-head", "origin", "--auto"); err != nil {
		return err
	}
	next := originBranch(ctx, t)
	if next == "" {
		return fmt.Errorf("Could not determine the default branch of %s", t.Name) //nolint:staticcheck // brew's wording
	}
	if current == "" || current == next {
		return nil
	}
	for _, args := range [][]string{
		{"config", "remote.origin.fetch", fetchRefspec},
		{"branch", "-m", current, next},
		{"branch", "-u", "origin/" + next, next},
	} {
		if _, err := run(ctx, t.Dir, args...); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintf(log, "==> %s: changed default branch name from %s to %s!\n", t.Name, current, next)
	return nil
}

func originBranch(ctx context.Context, t Tap) string {
	ref, err := run(ctx, t.Dir, "symbolic-ref", "-q", "refs/remotes/origin/HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(ref, "refs/remotes/origin/")
}

// Contents counts what the tap provides, as brew says it when tapping and untapping.
func (t Tap) Contents() []string {
	var out []string
	count := func(n int, one, many string) {
		if n == 1 {
			out = append(out, "1 "+one)
		} else if n > 1 {
			out = append(out, fmt.Sprintf("%d %s", n, many))
		}
	}
	commands, _ := filepath.Glob(filepath.Join(t.Dir, "cmd", "brew-*"))
	count(len(commands), "command", "commands")
	casks, _ := t.caskFiles()
	count(len(casks), "cask", "casks")
	formulae, _ := t.FormulaFiles()
	count(len(formulae), "formula", "formulae")
	return out
}

type linkDirs struct{ src, dst string }

func manpageDirs(prefix string, t Tap) []linkDirs {
	return []linkDirs{{filepath.Join(t.Dir, "manpages"), filepath.Join(prefix, "share/man/man1")}}
}

func completionDirs(prefix string, t Tap) []linkDirs {
	return []linkDirs{
		{filepath.Join(t.Dir, "completions/bash"), filepath.Join(prefix, "etc/bash_completion.d")},
		{filepath.Join(t.Dir, "completions/zsh"), filepath.Join(prefix, "share/zsh/site-functions")},
		{filepath.Join(t.Dir, "completions/fish"), filepath.Join(prefix, "share/fish/vendor_completions.d")},
	}
}

// LinkManpages links the tap's man pages into the prefix, and its shell completions too when it is an official checkout, as brew does.
func LinkManpages(root *fsx.Root, prefix string, t Tap, warn io.Writer) error {
	dirs := manpageDirs(prefix, t)
	if official(t.Name) && trust.SameRemote(t.Remote(), DefaultRemote(t.Name)) {
		dirs = append(dirs, completionDirs(prefix, t)...)
	}
	var errs []error
	for _, d := range dirs {
		errs = append(errs, linkAll(root, d, warn))
	}
	return errors.Join(errs...)
}

// UnlinkManpages removes the links the tap's man pages and completions made.
func UnlinkManpages(root *fsx.Root, prefix string, t Tap) error {
	var errs []error
	for _, d := range append(manpageDirs(prefix, t), completionDirs(prefix, t)...) {
		errs = append(errs, unlinkAll(root, d))
	}
	return errors.Join(errs...)
}

func filesUnder(dir string) (rels []string, err error) {
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			rels = append(rels, rel)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return rels, err
}

func sameFile(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

func isLink(root *fsx.Root, path string) bool {
	fi, err := root.Lstat(path)
	return err == nil && fi.Mode()&fs.ModeSymlink != 0
}

func linkAll(root *fsx.Root, d linkDirs, warn io.Writer) error {
	rels, err := filesUnder(d.src)
	if err != nil {
		return err
	}
	var conflicts []string
	for _, rel := range rels {
		src, dst := filepath.Join(d.src, rel), filepath.Join(d.dst, rel)
		if isLink(root, dst) {
			if sameFile(dst, src) {
				continue
			}
			if err := root.Remove(dst); err != nil {
				return err
			}
		}
		if _, err := root.Lstat(dst); err == nil {
			conflicts = append(conflicts, dst)
			continue
		}
		if err := root.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
			return err
		}
		target, err := filepath.Rel(filepath.Dir(dst), src)
		if err != nil {
			return err
		}
		if err := root.Symlink(target, dst); err != nil {
			return err
		}
	}
	if len(conflicts) > 0 {
		_, _ = fmt.Fprintf(warn, "brw: warning: could not link:\n  %s\nDelete these paths and tap again.\n", strings.Join(conflicts, "\n  "))
	}
	return nil
}

func unlinkAll(root *fsx.Root, d linkDirs) error {
	rels, err := filesUnder(d.src)
	if err != nil {
		return err
	}
	for _, rel := range rels {
		dst := filepath.Join(d.dst, rel)
		if isLink(root, dst) && sameFile(dst, filepath.Join(d.src, rel)) {
			if err := root.Remove(dst); err != nil {
				return err
			}
		}
		parent := filepath.Dir(dst)
		if entries, err := root.ReadDir(parent); err == nil && len(entries) == 0 {
			_ = root.Remove(parent)
		}
	}
	return nil
}

// CacheDir is where brew keeps its caches.
func CacheDir() string {
	if d := os.Getenv("HOMEBREW_CACHE"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library/Caches/Homebrew")
	}
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, "Homebrew")
	}
	return filepath.Join(home, ".cache/Homebrew")
}

// DescriptionCaches are brew's caches of formula and cask descriptions, keyed by full name.
var DescriptionCaches = []string{"descriptions.json", "cask_descriptions.json"}

// ForgetDescriptions drops the tap's entries from brew's description caches, as brew does when untapping.
func ForgetDescriptions(cacheDir string, t Tap) error {
	var errs []error
	for _, name := range DescriptionCaches {
		errs = append(errs, dropKeys(filepath.Join(cacheDir, name), t.Name+"/"))
	}
	return errors.Join(errs...)
}

type cacheEntry struct {
	key   string
	value json.RawMessage
}

func dropKeys(path, prefix string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	if tok, err := d.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	var kept []cacheEntry
	dropped := false
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return nil
		}
		key, _ := tok.(string)
		var v json.RawMessage
		if err := d.Decode(&v); err != nil {
			return nil
		}
		if strings.HasPrefix(strings.ToLower(key), prefix) {
			dropped = true
			continue
		}
		kept = append(kept, cacheEntry{key, v})
	}
	if !dropped {
		return nil
	}
	var out bytes.Buffer
	out.WriteByte('{')
	for i, en := range kept {
		if i > 0 {
			out.WriteByte(',')
		}
		k := &bytes.Buffer{}
		enc := json.NewEncoder(k)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(en.key)
		out.Write(bytes.TrimSpace(k.Bytes()))
		out.WriteByte(':')
		out.Write(en.value)
	}
	out.WriteByte('}')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".descriptions-*") //fsx:allow brew's cache, outside the prefix
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() //fsx:allow the temporary file, once renamed or failed
	if _, err := tmp.Write(out.Bytes()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(cachePerm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path) //fsx:allow brew's cache, outside the prefix
}
