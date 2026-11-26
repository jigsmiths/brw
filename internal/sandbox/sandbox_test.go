package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProfileDeniesNetworkUnlessAllowed(t *testing.T) {
	var s Sandbox
	s.DenyAllNetwork()
	p := s.Profile()
	for _, want := range []string{"(deny network*)\n", "(deny network-outbound (to unix-socket))\n", "(deny file-write*) ; deny non-allowlist file write operations\n", "(allow default)"} {
		if !strings.Contains(p, want) {
			t.Errorf("profile lacks %q", want)
		}
	}
	for _, unwanted := range []string{"mDNSResponder", "com.apple.trustd\"", "TrustEvaluationAgent"} {
		if strings.Contains(p, unwanted) {
			t.Errorf("a profile without network access has %q", unwanted)
		}
	}
	open := (&Sandbox{}).Profile()
	for _, want := range []string{`(allow network-outbound (to unix-socket (path-literal "/private/var/run/mDNSResponder")))`, `(global-name "com.apple.trustd")`, `(global-name "com.apple.TrustEvaluationAgent")`} {
		if !strings.Contains(open, want) {
			t.Errorf("a profile with network access lacks %q", want)
		}
	}
}

func TestProfileRulesAreRenderedInOrderAndQuoted(t *testing.T) {
	dir := t.TempDir()
	odd := filepath.Join(dir, `a "b" \c`)
	var s Sandbox
	s.AllowRead(dir)
	s.AllowWritePath(odd)
	s.DenyWriteLiteral(filepath.Join(dir, "brew"))
	s.AllowWriteSystemTemp(dir)
	p := s.Profile()
	real, _ := filepath.EvalSymlinks(dir)
	want := []string{
		`(allow file-read* (subpath "` + real + `"))`,
		`(allow file-write* (subpath "` + real + `/a \"b\" \\c"))`,
		`(deny file-write* (literal "` + real + `/brew"))`,
		`(allow file-write* (subpath "/private/tmp"))`,
		`(allow file-write* (regex #"^/private/var/folders/[^/]+/[^/]+/[C,T]/"))`,
	}
	last := -1
	for _, w := range want {
		i := strings.Index(p, w)
		if i < 0 || i < last {
			t.Fatalf("rule %q is missing or out of order in:\n%s", w, p)
		}
		last = i
	}
}

func TestDenyReadHomeDeniesTheWholeHomeUnlessAPathNeededIsInsideIt(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "Documents", "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(home)

	var outside Sandbox
	outside.DenyReadHome(home, []string{t.TempDir()}, func(string) { t.Error("unexpected warning") })
	if p := outside.Profile(); !strings.Contains(p, `(deny file-read* (subpath "`+real+`"))`) {
		t.Errorf("the whole home is not denied:\n%s", p)
	}

	var warned []string
	var inside Sandbox
	inside.DenyReadHome(home, []string{filepath.Join(home, "Documents", "work")}, func(m string) { warned = append(warned, m) })
	p := inside.Profile()
	if !strings.Contains(p, `(deny file-read* (subpath "`+real+`/.ssh"))`) {
		t.Errorf("the credentials are not denied:\n%s", p)
	}
	if strings.Contains(p, `(deny file-read* (subpath "`+real+`"))`) {
		t.Error("the whole home is denied though a needed path is inside it")
	}
	if strings.Contains(p, "Documents") || len(warned) != 1 || !strings.Contains(warned[0], "The sandbox cannot prevent formulae from reading:") {
		t.Errorf("Documents holds a needed path, so brew warns instead of denying: %q", warned)
	}
}

func TestDenyWriteHomebrewRepository(t *testing.T) {
	repo := t.TempDir()
	var separate Sandbox
	separate.DenyWriteHomebrewRepository(filepath.Join(repo, "bin/brew"), t.TempDir(), repo)
	if p := separate.Profile(); !strings.Contains(p, `(deny file-write* (subpath "`+realOf(repo)+`"))`) {
		t.Errorf("a repository outside the prefix is denied whole:\n%s", p)
	}
	prefix := filepath.Join(repo, "prefix")
	if err := os.MkdirAll(prefix, 0o755); err != nil {
		t.Fatal(err)
	}
	var shared Sandbox
	shared.DenyWriteHomebrewRepository(filepath.Join(repo, "bin/brew"), prefix, repo)
	p := shared.Profile()
	if !strings.Contains(p, `(deny file-write* (subpath "`+realOf(repo)+`/Library"))`) || !strings.Contains(p, `/.git"))`) {
		t.Errorf("a prefix inside the repository protects Library and .git only:\n%s", p)
	}
}

func realOf(p string) string {
	r, _ := filepath.EvalSymlinks(p)
	return r
}

func TestSandboxExecEnforcesTheProfileOnMacOS(t *testing.T) {
	if runtime.GOOS != "darwin" || !Available() {
		t.Skip("sandbox-exec is the point of this test and only macOS has it")
	}
	allowed, denied := t.TempDir(), t.TempDir()
	var s Sandbox
	s.AllowWritePath(allowed)
	run := func(dir string) error {
		return exec.Command(Exec, "-p", s.Profile(), "/usr/bin/touch", filepath.Join(dir, "f")).Run()
	}
	if err := run(allowed); err != nil {
		t.Skipf("the sandbox cannot be applied here (nested sandbox?): %v", err)
	}
	if err := run(denied); err == nil {
		t.Error("a write outside the allowed paths succeeded")
	}
	if _, err := os.Stat(filepath.Join(denied, "f")); err == nil {
		t.Error("the denied file exists")
	}
}
