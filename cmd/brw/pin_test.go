package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/install/installtest"
)

// pinStore has app installed and other known but not installed.
func pinStore(t *testing.T) (*env, string) {
	t.Helper()
	s := newStore(t)
	for _, name := range []string{"app", "other"} {
		s.formula(installtest.Bottle{Name: name, Version: "1.0", Files: []installtest.File{file("bin/"+name, name, 0o755)}}, nil, nil)
	}
	e, prefix := s.env()
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "app"); err != nil {
		t.Fatal(err)
	}
	e.err.(*bytes.Buffer).Reset()
	return e, prefix
}

func stderr(e *env) string { return e.err.(*bytes.Buffer).String() }

func record(t *testing.T, prefix, name string) string {
	t.Helper()
	got, err := os.Readlink(filepath.Join(prefix, "var/homebrew/pinned", name))
	if err != nil {
		return ""
	}
	return got
}

func TestPin(t *testing.T) {
	e, prefix := pinStore(t)
	out, err := runCmd(t, e, "pin", "-prefix", prefix, "app")
	if err != nil || out != "" || stderr(e) != "" {
		t.Fatalf("pin: %q, %v, stderr %q", out, err, stderr(e))
	}
	if got := record(t, prefix, "app"); got != "../../../Cellar/app/1.0" {
		t.Errorf("record -> %q", got)
	}
	out, err = runCmd(t, e, "pin", "-prefix", prefix, "app")
	if err != nil || out != "" || stderr(e) != "brw: warning: app already pinned\n" {
		t.Errorf("pin again: %q, %v, stderr %q", out, err, stderr(e))
	}
}

func TestPinTakesTheNewestKeg(t *testing.T) {
	e, prefix := pinStore(t)
	for _, v := range []string{"1.9", "1.10"} {
		if err := os.CopyFS(filepath.Join(prefix, "Cellar/app", v), os.DirFS(filepath.Join(prefix, "Cellar/app/1.0"))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runCmd(t, e, "pin", "-prefix", prefix, "app"); err != nil {
		t.Fatal(err)
	}
	if got := record(t, prefix, "app"); got != "../../../Cellar/app/1.10" {
		t.Errorf("record -> %q", got)
	}
}

func TestPinNames(t *testing.T) {
	e, prefix := pinStore(t)
	for _, name := range []string{"APP", "homebrew/core/app"} {
		if _, err := runCmd(t, e, "pin", "-prefix", prefix, name); err != nil {
			t.Errorf("pin %s: %v", name, err)
		}
	}
	if record(t, prefix, "app") == "" {
		t.Error("app was not pinned")
	}
}

func TestPinNotInstalled(t *testing.T) {
	e, prefix := pinStore(t)
	_, err := runCmd(t, e, "pin", "-prefix", prefix, "other", "app")
	if err == nil || err.Error() != "other not installed" {
		t.Errorf("error = %v, want other not installed", err)
	}
	if record(t, prefix, "app") == "" || record(t, prefix, "other") != "" {
		t.Error("the formula that is installed is pinned anyway, the other is not")
	}
}

func TestPinUnknownNameChangesNothing(t *testing.T) {
	e, prefix := pinStore(t)
	if _, err := runCmd(t, e, "pin", "-prefix", prefix, "app", "nosuch"); err == nil || !strings.Contains(err.Error(), "nosuch") {
		t.Errorf("error = %v", err)
	}
	if exists(t, prefix, "var/homebrew/pinned") {
		t.Error("something was pinned although a name was unknown")
	}
	if _, err := runCmd(t, e, "pin", "-prefix", prefix); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("no names: %v", err)
	}
}

func TestPinIsJournaled(t *testing.T) {
	e, prefix := pinStore(t)
	journal := filepath.Join(t.TempDir(), "journal")
	if _, err := runCmd(t, e, "pin", "-prefix", prefix, "-journal", journal, "app"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(journal); !strings.Contains(string(b), "symlink var/homebrew/pinned/app -> ../../../Cellar/app/1.0") {
		t.Errorf("journal = %q", b)
	}
}

func TestUnpin(t *testing.T) {
	e, prefix := pinStore(t)
	if _, err := runCmd(t, e, "pin", "-prefix", prefix, "app"); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, e, "unpin", "-prefix", prefix, "app")
	if err != nil || out != "" || stderr(e) != "" {
		t.Fatalf("unpin: %q, %v, stderr %q", out, err, stderr(e))
	}
	if exists(t, prefix, "var/homebrew/pinned") || !exists(t, prefix, "var/homebrew") {
		t.Error("the record and its empty directory should be gone, var/homebrew should not")
	}
	out, err = runCmd(t, e, "unpin", "-prefix", prefix, "app")
	if err != nil || out != "" || stderr(e) != "brw: warning: app not pinned\n" {
		t.Errorf("unpin again: %q, %v, stderr %q", out, err, stderr(e))
	}
}

func TestUnpinNotInstalledIsNotAFailure(t *testing.T) {
	e, prefix := pinStore(t)
	if _, err := runCmd(t, e, "unpin", "-prefix", prefix, "other"); err != nil {
		t.Errorf("brew does not fail here either: %v", err)
	}
	if got := stderr(e); got != "brw: other not installed\n" {
		t.Errorf("stderr = %q", got)
	}
	if _, err := runCmd(t, e, "unpin", "-prefix", prefix, "nosuch"); err == nil {
		t.Error("an unknown name should fail")
	}
	if _, err := runCmd(t, e, "unpin", "-prefix", prefix); err == nil {
		t.Error("no names should fail")
	}
}

func TestListPinned(t *testing.T) {
	e, prefix := pinStore(t)
	out, err := runCmd(t, e, "list", "-prefix", prefix, "--pinned")
	if err != nil || out != "" {
		t.Fatalf("nothing pinned: %q, %v", out, err)
	}
	if _, err := runCmd(t, e, "pin", "-prefix", prefix, "app"); err != nil {
		t.Fatal(err)
	}
	for args, want := range map[string]string{"--pinned": "app\n", "--pinned --versions": "app 1.0\n"} {
		out, err := runCmd(t, e, "list", append([]string{"-prefix", prefix}, strings.Fields(args)...)...)
		if err != nil || out != want {
			t.Errorf("list %s = %q, %v; want %q", args, out, err, want)
		}
	}
	if _, err := runCmd(t, e, "list", "-prefix", prefix, "--pinned", "app"); err == nil {
		t.Error("names are not supported with --pinned yet")
	}
}

func TestPinnedFormulaIsNotUninstalledOrUpgraded(t *testing.T) {
	e, prefix := pinStore(t)
	if _, err := runCmd(t, e, "pin", "-prefix", prefix, "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, e, "uninstall", "-prefix", prefix, "app"); err != nil && !strings.Contains(stderr(e), "pinned") {
		t.Errorf("uninstall: %v, stderr %q", err, stderr(e))
	}
	if !exists(t, prefix, "Cellar/app/1.0") {
		t.Error("a pinned formula was uninstalled")
	}
}
