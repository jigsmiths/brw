package main

import (
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Needs BRW_CONFORMANCE_FULL=1; on Linux (just conformance-linux) also $BRW_FULL_PREFIX, $BRW_FULL_NAMES, $BRW_BIN and optionally $BRW_FULL_REPORT.

func TestConformanceFull(t *testing.T) {
	if os.Getenv("BRW_CONFORMANCE_FULL") == "" {
		t.Skip("set BRW_CONFORMANCE_FULL=1 to install everything into a scratch prefix")
	}
	home, _ := os.UserHomeDir()
	oracle := cmpOr(os.Getenv("BRW_ORACLE"), filepath.Join(home, ".brw-oracle", "hb"))
	bin := os.Getenv("BRW_BIN")
	if bin == "" {
		bin = filepath.Join(t.TempDir(), "brw")
		sh(t, "", nil, "go", "build", "-o", bin, ".")
	}
	report := cmpOr(os.Getenv("BRW_FULL_REPORT"), filepath.Join(filepath.Dir(oracle), "full-report.txt"))

	var theirs, mine, work string
	var env []string
	if runtime.GOOS == "darwin" {
		work = workDir(t, filepath.Dir(oracle))
		theirs, mine = filepath.Join(work, "brew"), filepath.Join(work, "brw")
		for _, p := range []string{theirs, mine} {
			sh(t, "", nil, "git", "clone", "-q", oracle, p)
		}
		// A stale shared listing would make the two install different versions.
		refresh := filepath.Join(work, "refresh")
		sh(t, "", nil, "git", "clone", "-q", oracle, refresh)
		sh(t, "", []string{"HOMEBREW_NO_ENV_HINTS=1", "HOMEBREW_NO_ANALYTICS=1", "HOMEBREW_CACHE=" + filepath.Join(filepath.Dir(oracle), "cache")},
			filepath.Join(refresh, "bin", "brew"), "update")
		env = []string{"HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_ENV_HINTS=1", "HOMEBREW_NO_ANALYTICS=1",
			"HOMEBREW_CACHE=" + filepath.Join(filepath.Dir(oracle), "cache")}
	} else {
		work = t.TempDir()
		mine = cmpOr(os.Getenv("BRW_FULL_PREFIX"), "/home/linuxbrew/.linuxbrew")
	}
	brwEnv := append(slices.Clone(env), "HOME="+filepath.Join(work, "home"))
	brewIn := func(prefix string, args ...string) string {
		return sh(t, "", env, filepath.Join(prefix, "bin", "brew"), args...)
	}
	baseline := snapshot(t, mine)

	var eligible []string
	var left []string
	for _, name := range fullRunNames(t) {
		out, err := try(brwEnv, bin, "install", "-n", "-prefix", mine, name)
		if err != nil {
			left = append(left, fmt.Sprintf("%-28s %s", name, firstLine([]byte(out))))
			continue
		}
		eligible = append(eligible, name)
	}
	t.Logf("%d formulae to install, %d left out", len(eligible), len(left))

	start := time.Now()
	sh(t, "", brwEnv, bin, append([]string{"install", "-prefix", mine}, eligible...)...)
	brwTook := time.Since(start)
	var brewTook time.Duration
	if theirs != "" {
		start = time.Now()
		brewIn(theirs, append([]string{"install"}, eligible...)...)
		brewTook = time.Since(start)
	}

	var notes []string
	if theirs != "" {
		compare(t, "after install", theirs, mine)
		for _, name := range eligible {
			for _, d := range kegDiff(t, theirs, mine, name) {
				t.Errorf("%s keg: %s", name, d)
			}
			ra, rb := receipt(t, theirs, name), receipt(t, mine, name)
			for _, d := range lineDiff(strings.Join(ra, "\n"), strings.Join(rb, "\n")) {
				t.Errorf("%s receipt: %.300s", name, d)
			}
		}
	}
	for _, name := range eligible {
		s := smokeFormula(mine, name)
		for _, p := range s.Problems {
			t.Errorf("%s: %s", name, p)
		}
		for _, n := range s.Notes {
			notes = append(notes, name+": "+n)
		}
	}

	if theirs != "" {
		want := sh(t, "", brwEnv, bin, "list", "-prefix", mine)
		if got := brewIn(mine, "list", "--formula"); got != want {
			t.Errorf("brew list differs from brw list on brw's prefix\nbrew:\n%s\nbrw:\n%s", got, want)
		}
		brewIn(mine, append([]string{"info", "--json=v1"}, eligible...)...)
		doctor := func(prefix string) string {
			out, _ := try(env, filepath.Join(prefix, "bin", "brew"), "doctor")
			return strings.ReplaceAll(out, prefix, "<prefix>")
		}
		for _, d := range lineDiff(doctor(theirs), doctor(mine)) {
			t.Errorf("brew doctor differs: %s", d)
		}
	}

	// brew leaves some files behind on uninstall, such as formulae's etc files; brw must leave the same.
	sh(t, "", brwEnv, bin, append([]string{"uninstall", "-prefix", mine, "--force", "--ignore-dependencies"}, eligible...)...)
	if theirs != "" {
		brewIn(theirs, append([]string{"uninstall", "--force", "--ignore-dependencies"}, eligible...)...)
		compare(t, "after uninstalling everything", theirs, mine)
	} else {
		notes = append(notes, "left after uninstalling everything:\n"+strings.Join(lineDiff(baseline, snapshot(t, mine)), "\n"))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "installed %d, left out %d\nbrw install took %s, brew %s\n\nleft out:\n%s\n\nnotes:\n%s\n",
		len(eligible), len(left), brwTook.Round(time.Second), brewTook.Round(time.Second),
		strings.Join(left, "\n"), strings.Join(notes, "\n"))
	if err := os.WriteFile(report, []byte(b.String()), 0o644); err != nil {
		t.Error(err)
	}
	t.Logf("report written to %s", report)
}

// Only stdout counts; brew warns about other taps on stderr.
func fullRunNames(t *testing.T) []string {
	t.Helper()
	var out []byte
	var err error
	if f := os.Getenv("BRW_FULL_NAMES"); f != "" {
		out, err = os.ReadFile(f)
	} else {
		out, err = osexec.Command("brew", "list", "--formula", "-1").Output()
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(out))
}
