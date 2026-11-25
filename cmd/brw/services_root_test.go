package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

type ownership struct {
	paths  []string
	group  string
	sticky bool
}

type rootFakes struct{ owned []ownership }

func rootEnvWith(t *testing.T) (*env, string, string, *fakeLaunchctl, *rootFakes) {
	return rootEnvFor(t, testPlist)
}

func rootEnvFor(t *testing.T, plist string) (*env, string, string, *fakeLaunchctl, *rootFakes) {
	t.Helper()
	e, prefix, agents, lc := servicesEnvWith(t, "sh.brew.foo.plist", plist)
	return e, prefix, agents, lc, asRoot(e)
}

func asRoot(e *env) *rootFakes {
	e.uid = 0
	fakes := &rootFakes{}
	e.takeOwnership = func(paths []string, group string, sticky bool) error {
		fakes.owned = append(fakes.owned, ownership{slices.Clone(paths), group, sticky})
		return nil
	}
	e.userExists = func(name string) bool { return name == "svc" || name == e.user }
	return fakes
}

func TestOwnerChangerSetsOwnershipAndTheStickyBit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keg")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(t.TempDir(), "x.plist")
	if err := os.WriteFile(plist, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var group string
	o := ownerChanger{uid: os.Getuid(), lookupGid: func(name string) (int, error) { group = name; return os.Getgid(), nil }}

	if err := o.apply([]string{plist}, "admin", false); err != nil {
		t.Fatal(err)
	}
	if group != "admin" {
		t.Errorf("group looked up = %q", group)
	}
	fi, _ := os.Stat(plist)
	if fi.Mode().Perm() != 0o644 || fi.Mode()&os.ModeSticky != 0 {
		t.Errorf("plist mode = %v", fi.Mode())
	}
	if st := fi.Sys().(*syscall.Stat_t); int(st.Uid) != os.Getuid() || int(st.Gid) != os.Getgid() {
		t.Errorf("plist owner = %d:%d", st.Uid, st.Gid)
	}

	if err := o.apply([]string{dir}, "admin", true); err != nil {
		t.Fatal(err)
	}
	fi, _ = os.Stat(dir)
	if fi.Mode()&os.ModeSticky == 0 || fi.Mode().Perm() != 0o750 {
		t.Errorf("keg mode = %v, want sticky with 0750 kept", fi.Mode())
	}
	if st := fi.Sys().(*syscall.Stat_t); int(st.Uid) != os.Getuid() || int(st.Gid) != os.Getgid() {
		t.Errorf("keg owner = %d:%d", st.Uid, st.Gid)
	}

	if err := o.apply([]string{filepath.Join(dir, "missing")}, "admin", true); err == nil {
		t.Error("a missing path is an error")
	}
	bad := ownerChanger{uid: os.Getuid(), lookupGid: func(string) (int, error) { return 0, errors.New("no group") }}
	if err := bad.apply([]string{dir}, "admin", false); err == nil {
		t.Error("an unknown group is an error")
	}
}

func TestServicesRootStartUsesTheDaemonsDirAndSystemDomain(t *testing.T) {
	e, prefix, agents, lc, fakes := rootEnvWith(t)
	out, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo")
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(e.daemonsDir, "sh.brew.foo.plist")
	if got, err := os.ReadFile(dst); err != nil || string(got) != testPlist {
		t.Fatalf("daemon plist = %q, %v", got, err)
	}
	if entries, _ := os.ReadDir(agents); len(entries) != 0 {
		t.Errorf("root writes no agents: %v", entries)
	}
	if want := "enable system/sh.brew.foo\nbootstrap system " + dst; calls(lc) != want {
		t.Errorf("calls = %q, want %q", calls(lc), want)
	}
	if want := "==> Successfully started `foo` (label: sh.brew.foo)\n"; out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
	if !strings.HasSuffix(stderr(e), "Warning: `foo` must be run as non-root to start at user login!\n"+
		"The service will run as root unless configured otherwise.\n") {
		t.Errorf("stderr = %q", stderr(e))
	}
	if len(fakes.owned) != 2 || fakes.owned[0].paths[0] != dst || fakes.owned[0].group != "admin" || fakes.owned[0].sticky {
		t.Errorf("the plist goes to root:admin first: %+v", fakes.owned)
	}
}

func TestServicesRootStartTakesOwnershipOfTheServiceSoftware(t *testing.T) {
	e, prefix, _, _, fakes := rootEnvWith(t)
	if _, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	keg := filepath.Join(prefix, "Cellar/foo/1.0")
	wantPaths := []string{filepath.Join(prefix, "opt/foo"), filepath.Join(prefix, "var/homebrew/linked/foo"), filepath.Join(keg, "bin")}
	slices.Sort(wantPaths)
	wantPaths = slices.DeleteFunc(wantPaths, func(p string) bool { _, err := os.Stat(p); return err != nil })
	got := fakes.owned[1]
	if !slices.Equal(got.paths, wantPaths) || got.group != "admin" || !got.sticky {
		t.Errorf("owned = %+v, want %v", got, wantPaths)
	}
	if len(wantPaths) < 2 {
		t.Fatalf("test prefix has too few paths: %v", wantPaths)
	}
	warning := "Warning: Taking root:admin ownership of some foo paths:\n  " + strings.Join(wantPaths, "\n  ") + "\n" +
		"This will require manual removal of these paths using `sudo rm` on\n" +
		"brw upgrade/reinstall/uninstall.\n" +
		"Root services will run Homebrew-installed software as root by default. Only use\n" +
		"root services if you trust all users who can write to your Homebrew installation.\n"
	if !strings.Contains(stderr(e), warning) {
		t.Errorf("stderr = %q, want it to contain %q", stderr(e), warning)
	}
}

func TestServicesRootStartOwnsTheProgramAndItsDirectory(t *testing.T) {
	plist := "<plist version=\"1.0\">\n<dict>\n\t<key>Label</key>\n\t<string>sh.brew.foo</string>\n" +
		"\t<key>ProgramArguments</key>\n\t<array>\n\t\t<string>%s</string>\n\t</array>\n</dict>\n</plist>\n"
	program := filepath.Join(t.TempDir(), "bin", "daemon")
	if err := os.MkdirAll(filepath.Dir(program), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(program, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	e, prefix, _, _, fakes := rootEnvWith(t)
	source := filepath.Join(prefix, "Cellar/foo/1.0/sh.brew.foo.plist")
	if err := os.WriteFile(source, []byte(strings.Replace(plist, "%s", program, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(program)
	for _, want := range []string{real, filepath.Dir(real)} {
		if !slices.Contains(fakes.owned[1].paths, want) {
			t.Errorf("owned %v lacks %s", fakes.owned[1].paths, want)
		}
	}
}

func TestServicesRootStartWarnsWhenTheProgramIsMissing(t *testing.T) {
	e, prefix, _, _, _ := rootEnvWith(t)
	source := filepath.Join(prefix, "Cellar/foo/1.0/sh.brew.foo.plist")
	body := "<plist version=\"1.0\">\n<dict>\n\t<key>ProgramArguments</key>\n\t<array>\n\t\t<string>/no/such/daemon</string>\n\t</array>\n</dict>\n</plist>\n"
	if err := os.WriteFile(source, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	if want := "Warning: foo: the first ProgramArguments value does not exist:\n  /no/such/daemon\n"; !strings.Contains(stderr(e), want) {
		t.Errorf("stderr = %q, want %q", stderr(e), want)
	}
}

func TestServicesSudoServiceUserRunsTheServiceAsThatUser(t *testing.T) {
	e, prefix, _, lc, fakes := rootEnvFor(t, envPlist)
	out, err := runCmd(t, e, "services", "start", "-prefix", prefix, "--sudo-service-user", "svc", "foo")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(e.daemonsDir, "sh.brew.foo.plist"))
	if !strings.Contains(string(body), "<key>UserName</key>\n\t<string>svc</string>") {
		t.Errorf("plist lacks the user:\n%s", body)
	}
	if want := "==> Setting username in sh.brew.foo to: svc\n==> Successfully started `foo` (label: sh.brew.foo)\n"; out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
	if len(fakes.owned) != 0 {
		t.Errorf("a service run as another user keeps its ownership: %+v", fakes.owned)
	}
	if stderr(e) != "" {
		t.Errorf("stderr = %q", stderr(e))
	}
	if !strings.HasPrefix(calls(lc), "enable system/sh.brew.foo") {
		t.Errorf("calls = %q", calls(lc))
	}
}

func TestServicesSudoServiceUserMustExist(t *testing.T) {
	e, prefix, _, lc, _ := rootEnvFor(t, envPlist)
	_, err := runCmd(t, e, "services", "start", "-prefix", prefix, "--sudo-service-user=ghost", "foo")
	if err == nil || err.Error() != "Cannot start `foo` as `ghost` is not a user!" || len(lc.calls) != 0 {
		t.Errorf("err = %v, calls = %q", err, lc.calls)
	}
}

func TestServicesSudoServiceUserNeedsRoot(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	for _, sub := range []string{"list", "start", "stop", "restart"} {
		args := []string{sub, "-prefix", prefix, "--sudo-service-user", "svc"}
		if sub != "list" {
			args = append(args, "foo")
		}
		_, err := runCmd(t, e, "services", args...)
		if err == nil || !strings.Contains(err.Error(), "`brw services --sudo-service-user` is supported only when running as root!") {
			t.Errorf("%s: %v", sub, err)
		}
	}
	if len(lc.calls) != 0 {
		t.Errorf("calls = %q", lc.calls)
	}
}

func TestServicesStartWarnsAboutRequireRoot(t *testing.T) {
	formula := strings.Replace(explicitNameFormula, "    name macos: \"custom.foo\"\n", "    require_root true\n", 1)
	e, prefix, _, _ := servicesEnvFiles(t, file(".brew/foo.rb", formula, 0o644), file("sh.brew.foo.plist", testPlist, 0o644))
	if _, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	if want := "Warning: `foo` must be run as root to start at system startup!\n"; stderr(e) != want {
		t.Errorf("stderr = %q, want %q", stderr(e), want)
	}

	e.uid = 0
	e.takeOwnership = func([]string, string, bool) error { return nil }
	e.err.(interface{ Reset() }).Reset()
	if _, err := runCmd(t, e, "services", "restart", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr(e), "non-root") {
		t.Errorf("a service that needs root has nothing to warn about as root: %q", stderr(e))
	}
}

func TestServicesRootStopBootsOutOfTheSystemDomain(t *testing.T) {
	e, prefix, _, lc, _ := rootEnvWith(t)
	dst := filepath.Join(e.daemonsDir, "sh.brew.foo.plist")
	if err := os.WriteFile(dst, []byte(testPlist), 0o644); err != nil {
		t.Fatal(err)
	}
	lc.loaded["sh.brew.foo"] = runningOut
	out, err := runCmd(t, e, "services", "stop", "-prefix", prefix, "foo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(calls(lc), "bootout system/sh.brew.foo") || strings.Contains(calls(lc), "gui/") {
		t.Errorf("calls = %q", calls(lc))
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("stop removes the daemon plist: %v", err)
	}
	if want := "Stopping `foo`... (might take a while)\n==> Successfully stopped `foo` (label: sh.brew.foo)\n"; out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

func TestServicesStopSaysWhichUserStartedAServiceInAnotherDomain(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	if err := os.WriteFile(filepath.Join(e.daemonsDir, "sh.brew.foo.plist"), []byte(testPlist), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runCmd(t, e, "services", "stop", "-prefix", prefix, "foo")
	want := "Service `foo` is started as `root`. Try:\n  sudo brw services stop foo"
	if err == nil || err.Error() != want {
		t.Errorf("as user: %v, want %q", err, want)
	}
	if len(lc.calls) != 0 {
		t.Errorf("calls = %q", lc.calls)
	}

	if err := os.Remove(filepath.Join(e.daemonsDir, "sh.brew.foo.plist")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, "sh.brew.foo.plist"), []byte(testPlist), 0o644); err != nil {
		t.Fatal(err)
	}
	e.uid = 0
	_, err = runCmd(t, e, "services", "stop", "-prefix", prefix, "foo")
	want = "Service `foo` is started as `me`. Try:\n  brw services stop foo"
	if err == nil || err.Error() != want {
		t.Errorf("as root: %v, want %q", err, want)
	}
}

func TestServicesRootCannotRunAService(t *testing.T) {
	e, prefix, _, lc, _ := rootEnvWith(t)
	lc.loaded["sh.brew.foo"] = "\tstate = not running\n\tlast exit code = 0\n"
	out, err := runCmd(t, e, "services", "restart", "-prefix", prefix, "foo")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Service `foo` cannot be run (but can be started) as root.\n"; !strings.HasSuffix(out, want) {
		t.Errorf("out = %q, want suffix %q", out, want)
	}
	for _, c := range lc.calls {
		if strings.HasPrefix(c, "bootstrap") {
			t.Errorf("root must not run a service: %q", lc.calls)
		}
	}
}

func TestServicesRootDryRun(t *testing.T) {
	e, prefix, _, lc, _ := rootEnvWith(t)
	out, err := runCmd(t, e, "services", "restart", "-prefix", prefix, "--dry-run", "foo")
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(e.daemonsDir, "sh.brew.foo.plist")
	want := "launchctl bootout system/sh.brew.foo\nrm " + dst + " " + filepath.Join(e.daemonsDir, "homebrew.mxcl.foo.plist") + "\n" +
		"cp " + filepath.Join(prefix, "Cellar/foo/1.0/sh.brew.foo.plist") + " " + dst +
		"\nlaunchctl enable system/sh.brew.foo\nlaunchctl bootstrap system " + dst + "\n"
	if out != want || len(lc.calls) != 0 {
		t.Errorf("out = %q, want %q (calls %q)", out, want, lc.calls)
	}
}

func TestServicesListShowsRootDaemons(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	dst := filepath.Join(e.daemonsDir, "sh.brew.foo.plist")
	if err := os.WriteFile(dst, []byte(testPlist), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := runCmd(t, e, "services", "list", "-prefix", prefix)
	if want := "Name Status User File\nfoo  none            root \n"; out != want {
		t.Errorf("as user: %q, want %q", out, want)
	}

	e.uid = 0
	lc.loaded["sh.brew.foo"] = runningOut
	out, _ = runCmd(t, e, "services", "list", "-prefix", prefix)
	if want := "Name Status User File\nfoo  started         root " + dst + "\n"; out != want {
		t.Errorf("as root: %q, want %q", out, want)
	}
}

func TestServicesStartAsRootIgnoresTheUserEnvFile(t *testing.T) {
	e, prefix, agents, _ := servicesEnvFiles(t, file(".brew/foo.rb", envFormula, 0o644), file("sh.brew.foo.plist", envPlist, 0o644))
	asRoot(e)
	path := writeEnvFile(t, e, 0o600, "FOO=bar\n")
	if _, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(e.daemonsDir, "sh.brew.foo.plist"))
	if strings.Contains(string(got), "<key>FOO</key>") {
		t.Errorf("env was applied to a root service:\n%s", got)
	}
	if want := "Warning: Skipping " + path + ": user env overrides are not supported for root services.\n"; !strings.Contains(stderr(e), want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr(e), want)
	}
	if entries, _ := os.ReadDir(agents); len(entries) != 0 {
		t.Errorf("wrote %v", entries)
	}
}

func TestServicesListShowsTheServiceUserOfARootDaemon(t *testing.T) {
	e, prefix, _, _ := servicesEnv(t)
	body := "<plist><dict><key>Label</key><string>sh.brew.foo</string><key>UserName</key><string>svc</string></dict></plist>\n"
	if err := os.WriteFile(filepath.Join(e.daemonsDir, "sh.brew.foo.plist"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	e.uid = 0
	out, _ := runCmd(t, e, "services", "list", "--json", "-prefix", prefix)
	if !strings.Contains(out, "\"user\": \"svc\"") {
		t.Errorf("out = %s", out)
	}
}
