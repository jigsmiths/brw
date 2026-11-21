package tap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = GitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// publish writes a cask and the listing entry brew would generate for it.
func publish(t *testing.T, dir, token, version string) {
	t.Helper()
	rb := fmt.Sprintf("cask %q do\n  version %q\nend\n", token, version)
	write(t, filepath.Join(dir, "Casks", token+".rb"), rb)
	writeListing(t, dir, entryJSON(token, version, rb))
}

func entryJSON(token, version, rb string) string {
	sum := sha256.Sum256([]byte(rb))
	return fmt.Sprintf(`{"token": %q, "tap": "someone/else", "version": %q, "ruby_source_path": "Casks/%s.rb", "ruby_source_checksum": {"sha256": %q}}`,
		token, version, token, hex.EncodeToString(sum[:]))
}

func writeListing(t *testing.T, dir string, entries ...string) {
	t.Helper()
	list := "["
	for i, e := range entries {
		if i > 0 {
			list += ","
		}
		list += e
	}
	write(t, filepath.Join(dir, "api", "cask.json"), list+"]")
}

// origin makes a tap repository to clone from.
func origin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	publish(t, dir, "tool", "1.0")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "tool 1.0")
	return dir
}

func TestSplit(t *testing.T) {
	for name, want := range map[string][2]string{
		"jigsmiths/tap/brw":   {"jigsmiths/tap", "brw"},
		"JigSmiths/Tap/brw":   {"jigsmiths/tap", "brw"},
		"homebrew/cask/slack": {"homebrew/cask", "slack"},
	} {
		tap, token, ok := Split(name)
		if !ok || tap != want[0] || token != want[1] {
			t.Errorf("Split(%q) = %q, %q, %v", name, tap, token, ok)
		}
	}
	for _, name := range []string{"brw", "jigsmiths/brw", "a/b/c/d", "a//c", "/b/c"} {
		if _, _, ok := Split(name); ok {
			t.Errorf("Split(%q) should fail", name)
		}
	}
}

func TestList(t *testing.T) {
	prefix := t.TempDir()
	taps := filepath.Join(prefix, "Library", "Taps")
	for _, d := range []string{"jigsmiths/homebrew-tap", "oven-sh/homebrew-bun", "someone/not-a-tap", ".hidden/homebrew-x"} {
		if err := os.MkdirAll(filepath.Join(taps, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(taps, "stray"), "")
	got, err := List(prefix)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tp := range got {
		names = append(names, tp.Name)
		if tp.Dir != Dir(prefix, tp.Name) {
			t.Errorf("%s: Dir = %s", tp.Name, tp.Dir)
		}
	}
	if want := []string{"jigsmiths/tap", "oven-sh/bun"}; !slices.Equal(names, want) {
		t.Errorf("List = %q, want %q", names, want)
	}
	if got, err := List(t.TempDir()); err != nil || got != nil {
		t.Errorf("without taps: %v, %v", got, err)
	}
}

func TestCasks(t *testing.T) {
	dir := t.TempDir()
	tp := Tap{Name: "jigsmiths/tap", Dir: dir}
	if got, err := tp.Casks(); err != nil || got != nil {
		t.Errorf("without a listing: %v, %v", got, err)
	}
	rb := "cask \"tool\" do\nend\n"
	write(t, filepath.Join(dir, "Casks", "tool.rb"), rb)
	write(t, filepath.Join(dir, "Casks", "stale.rb"), "changed since the listing")
	writeListing(t, dir, entryJSON("tool", "1.0", rb), entryJSON("stale", "1.0", "as listed"), entryJSON("gone", "1.0", "x"))
	got, err := tp.Casks()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Token != "tool" {
		t.Fatalf("Casks = %+v, want just tool", got)
	}
	if got[0].Tap != "jigsmiths/tap" || got[0].SourceFile != filepath.Join(dir, "Casks", "tool.rb") {
		t.Errorf("tap %q, source file %q", got[0].Tap, got[0].SourceFile)
	}
	write(t, filepath.Join(dir, "api", "cask.json"), "{")
	if _, err := tp.Casks(); err == nil {
		t.Error("a garbled listing should be an error")
	}
}

func TestCloneAndPull(t *testing.T) {
	src := origin(t)
	prefix := t.TempDir()
	tp, err := Clone(context.Background(), prefix, "jigsmiths/tap", src)
	if err != nil {
		t.Fatal(err)
	}
	if tp.Name != "jigsmiths/tap" || tp.Dir != filepath.Join(prefix, "Library/Taps/jigsmiths/homebrew-tap") {
		t.Errorf("Clone = %+v", tp)
	}
	if remote := git(t, tp.Dir, "remote"); remote != "origin\n" {
		t.Errorf("remote = %q", remote)
	}
	if got := tp.Remote(); got != src {
		t.Errorf("Remote = %q, want %q", got, src)
	}
	if got := (Tap{Dir: t.TempDir()}).Remote(); got != "" {
		t.Errorf("Remote of a folder that is no clone = %q", got)
	}
	if _, err := Clone(context.Background(), prefix, "jigsmiths/tap", src); err == nil {
		t.Error("cloning a tap that is there should fail")
	}

	publish(t, src, "tool", "2.0")
	git(t, src, "commit", "-q", "-am", "tool 2.0")
	changed, err := Pull(context.Background(), tp)
	if err != nil || !changed {
		t.Fatalf("Pull = %v, %v", changed, err)
	}
	casks, err := tp.Casks()
	if err != nil || len(casks) != 1 || casks[0].Version != "2.0" {
		t.Fatalf("after pull: %+v, %v", casks, err)
	}
	// brew's receipt records the tap's HEAD, not the commit the listing names.
	if head := strings.TrimSpace(git(t, tp.Dir, "rev-parse", "HEAD")); casks[0].TapGitHead != head {
		t.Errorf("TapGitHead = %q, want %q", casks[0].TapGitHead, head)
	}
	if changed, err := Pull(context.Background(), tp); err != nil || changed {
		t.Errorf("Pull with nothing new = %v, %v", changed, err)
	}
}

// As inside a git hook, or with the user's own git settings.
func TestPullIgnoresTheCallersGitEnvironment(t *testing.T) {
	src := origin(t)
	tp, err := Clone(context.Background(), t.TempDir(), "jigsmiths/tap", src)
	if err != nil {
		t.Fatal(err)
	}
	publish(t, src, "tool", "2.0")
	git(t, src, "commit", "-q", "-am", "tool 2.0")
	stray := t.TempDir()
	t.Setenv("GIT_DIR", filepath.Join(stray, ".git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(stray, "index"))
	t.Setenv("GIT_WORK_TREE", stray)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "pull.rebase")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	if changed, err := Pull(context.Background(), tp); err != nil || !changed {
		t.Errorf("Pull = %v, %v", changed, err)
	}
	if entries, _ := os.ReadDir(stray); len(entries) != 0 {
		t.Errorf("git wrote to the caller's repository: %v", entries)
	}
}

func TestPullDoesNotRunHooks(t *testing.T) {
	src := origin(t)
	tp, err := Clone(context.Background(), t.TempDir(), "jigsmiths/tap", src)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	write(t, filepath.Join(tp.Dir, ".git", "hooks", "post-merge"), "#!/bin/sh\ntouch "+marker+"\n")
	if err := os.Chmod(filepath.Join(tp.Dir, ".git", "hooks", "post-merge"), 0o755); err != nil {
		t.Fatal(err)
	}
	publish(t, src, "tool", "2.0")
	git(t, src, "commit", "-q", "-am", "tool 2.0")
	if _, err := Pull(context.Background(), tp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a git hook in the tap ran")
	}
}
