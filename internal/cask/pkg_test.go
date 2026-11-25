package cask

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type fakeRun struct {
	calls  []Command
	handle func(c Command) ([]byte, error)
}

func (f *fakeRun) run(_ context.Context, c Command) ([]byte, error) {
	f.calls = append(f.calls, c)
	if f.handle != nil {
		return f.handle(c)
	}
	return nil, nil
}

func (f *fakeRun) sudoCalls() []Command {
	return slices.DeleteFunc(slices.Clone(f.calls), func(c Command) bool { return c.Name != "/usr/bin/sudo" })
}

func loginName(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return u.Username
}

func asRoot(t *testing.T) {
	t.Helper()
	old := geteuid
	geteuid = func() int { return 0 }
	t.Cleanup(func() { geteuid = old })
}

var pkgTarball = tarGzBytes(&testing.T{},
	entry{name: "Sub/P.pkg", body: "pkg"},
	entry{name: "Other.pkg", body: "pkg"},
	entry{name: ".hidden/H.pkg", body: "pkg"})

const choicesPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<array>
	<dict>
		<key>attributeSetting</key>
		<integer>1</integer>
		<key>choiceAttribute</key>
		<string>selected</string>
		<key>choiceIdentifier</key>
		<string>a&amp;b</string>
	</dict>
</array>
</plist>
`

func TestInstallPkg(t *testing.T) {
	ti := newTestInstall(t, pkgTarball)
	f := &fakeRun{}
	var choices string
	var choicesPath string
	f.handle = func(c Command) ([]byte, error) {
		if i := slices.Index(c.Args, "-applyChoiceChangesXML"); i >= 0 {
			choicesPath = c.Args[i+1]
			b, err := os.ReadFile(choicesPath)
			choices = string(b)
			return nil, err
		}
		return nil, nil
	}
	ti.in.Run = f.run
	c := ti.cask(t, `[
		{"pkg":["Sub/P.pkg",{"choices":[{"choiceIdentifier":"a&b","choiceAttribute":"selected","attributeSetting":1}]}]},
		{"uninstall":[{"pkgutil":"com.p"}]}]`)
	if err := ti.in.Install(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	u := loginName(t)
	staged := filepath.Join(ti.prefix, "Caskroom/app/1.0/Sub/P.pkg")
	want := []string{"-u", "root", "-E", "LOGNAME=" + u, "USER=" + u, "USERNAME=" + u, "--",
		"/usr/sbin/installer", "-pkg", staged, "-target", "/", "-applyChoiceChangesXML", choicesPath}
	calls := f.sudoCalls()
	if len(calls) != 1 || !slices.Equal(calls[0].Args, want) {
		t.Fatalf("sudo calls = %+v\nwant args %q", calls, want)
	}
	if choices != choicesPlist {
		t.Errorf("choices =\n%s\nwant\n%s", choices, choicesPlist)
	}
	if _, err := os.Stat(choicesPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the choices file should be removed: %v", err)
	}
	if !strings.Contains(ti.out.String(), "==> Running installer for app with `sudo` (which may request your password)...\n") {
		t.Errorf("out = %q", ti.out.String())
	}
	receipt, err := os.ReadFile(filepath.Join(ti.prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json"))
	if err != nil || strings.Contains(string(receipt), `"pkg"`) || !strings.Contains(string(receipt), `"pkgutil": "com.p"`) {
		t.Errorf("receipt = %s, %v", receipt, err)
	}
}

func TestInstallPkgWithoutChoicesOrWithEmptyOnes(t *testing.T) {
	for _, artifacts := range []string{
		`[{"pkg":["Sub/P.pkg"]}]`,
		`[{"pkg":["Sub/P.pkg",{"choices":[]}]}]`,
		`[{"pkg":["Sub/P.pkg",{"choices":{}}]}]`,
		`[{"pkg":["Sub/P.pkg",{"choices":null}]}]`,
	} {
		ti := newTestInstall(t, pkgTarball)
		f := &fakeRun{}
		ti.in.Run = f.run
		if err := ti.in.Install(context.Background(), ti.cask(t, artifacts)); err != nil {
			t.Fatal(err)
		}
		calls := f.sudoCalls()
		if len(calls) != 1 || slices.Contains(calls[0].Args, "-applyChoiceChangesXML") {
			t.Errorf("%s: calls = %+v", artifacts, calls)
		}
	}
}

func TestInstallPkgAllowUntrustedIsDeprecated(t *testing.T) {
	ti := newTestInstall(t, pkgTarball)
	f := &fakeRun{}
	ti.in.Run = f.run
	if err := ti.in.Install(context.Background(), ti.cask(t, `[{"pkg":["Sub/P.pkg",{"allow_untrusted":true}]}]`)); err != nil {
		t.Fatal(err)
	}
	if calls := f.sudoCalls(); len(calls) != 1 || !slices.Contains(calls[0].Args, "-allowUntrusted") {
		t.Errorf("calls = %+v", calls)
	}
	want := "Warning: Calling `allow_untrusted` in the `pkg` stanza is deprecated! Use a trusted package instead.\n"
	if ti.warn.String() != want {
		t.Errorf("warn = %q, want %q", ti.warn.String(), want)
	}
}

func TestInstallPkgMissingSource(t *testing.T) {
	ti := newTestInstall(t, pkgTarball)
	f := &fakeRun{}
	ti.in.Run = f.run
	err := ti.in.Install(context.Background(), ti.cask(t, `[{"pkg":["Missing.pkg"]}]`))
	want := "app: Could not find PKG source file 'Missing.pkg', found 'Other.pkg' and 'Sub/P.pkg' instead."
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	if len(f.calls) > 0 {
		t.Errorf("installer ran: %+v", f.calls)
	}
	if exists(filepath.Join(ti.prefix, "Caskroom/app")) {
		t.Error("the failed install should be cleaned up")
	}
}

func TestInstallPkgMissingSourceListsNothingWhenThereIsNone(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	err := ti.in.Install(context.Background(), ti.cask(t, `[{"pkg":["Missing.pkg"]}]`))
	if want := "app: Could not find PKG source file 'Missing.pkg'."; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

func TestInstallPkgRefusesToRunAsRoot(t *testing.T) {
	asRoot(t)
	ti := newTestInstall(t, pkgTarball)
	f := &fakeRun{}
	ti.in.Run = f.run
	err := ti.in.Install(context.Background(), ti.cask(t, `[{"pkg":["Sub/P.pkg"]}]`))
	if err == nil || !strings.Contains(err.Error(), "Running Homebrew as root is extremely dangerous and no longer supported.") {
		t.Errorf("err = %v", err)
	}
	if len(f.calls) > 0 {
		t.Errorf("ran %+v", f.calls)
	}
}

func TestInstallPkgFailureRollsBack(t *testing.T) {
	ti := newTestInstall(t, pkgTarball)
	f := &fakeRun{handle: func(Command) ([]byte, error) { return nil, errors.New("installer failed") }}
	ti.in.Run = f.run
	err := ti.in.Install(context.Background(), ti.cask(t, `[{"pkg":["Sub/P.pkg"]}]`))
	if err == nil || !strings.Contains(err.Error(), "installer failed") {
		t.Errorf("err = %v", err)
	}
	if exists(filepath.Join(ti.prefix, "Caskroom/app")) {
		t.Error("the failed install should be cleaned up")
	}
}

func TestInstallPkgVerbose(t *testing.T) {
	ti := newTestInstall(t, pkgTarball)
	f := &fakeRun{}
	ti.in.Run = f.run
	ti.in.Verbose = true
	if err := ti.in.Install(context.Background(), ti.cask(t, `[{"pkg":["Sub/P.pkg",{"allow_untrusted":true}]}]`)); err != nil {
		t.Fatal(err)
	}
	calls := f.sudoCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	i := slices.Index(calls[0].Args, "-target")
	if got := calls[0].Args[i+2 : i+4]; !slices.Equal(got, []string{"-verboseR", "-allowUntrusted"}) {
		t.Errorf("flags after -target / = %q", got)
	}
}

type bomPkg struct {
	plist string
	files []string
}

func pkgInfoPlist(volume, location string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>install-location</key>
	<string>%s</string>
	<key>pkg-version</key>
	<string>1.0</string>
	<key>pkgid</key>
	<string>com.example.a</string>
	<key>receipt-plist-version</key>
	<real>1.0</real>
	<key>volume</key>
	<string>%s</string>
</dict>
</plist>
`, location, volume)
}

func fakePkgutil(pkgs map[string]bomPkg, matches string) func(Command) ([]byte, error) {
	return func(c Command) ([]byte, error) {
		if c.Name != "/usr/bin/env" || c.Args[0] != "/usr/sbin/pkgutil" {
			return nil, nil
		}
		switch flag := c.Args[1]; {
		case strings.HasPrefix(flag, "--pkgs="):
			return []byte(matches), nil
		case flag == "--files":
			p, ok := pkgs[c.Args[2]]
			if !ok {
				return nil, errors.New("no such package")
			}
			return []byte(strings.Join(p.files, "\n") + "\n"), nil
		case flag == "--pkg-info-plist":
			return []byte(pkgs[c.Args[2]].plist), nil
		}
		return nil, nil
	}
}

func TestUninstallPkgutil(t *testing.T) {
	ti, _ := installed(t, `[{"app":["App.app"]}]`)
	receipt := filepath.Join(ti.prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json")
	if err := os.WriteFile(receipt, []byte(`{"uninstall_artifacts":[{"uninstall":[{"pkgutil":["com.example.*"]}]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	volume := t.TempDir()
	root := filepath.Join(volume, "opt/x")
	for _, d := range []string{"bin", "lib/deep"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"bin/tool", "lib/deep/f"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("tool", filepath.Join(root, "bin/link")); err != nil {
		t.Fatal(err)
	}
	f := &fakeRun{handle: fakePkgutil(map[string]bomPkg{
		"com.example.a": {plist: pkgInfoPlist(volume, "opt/x"), files: []string{"bin", "bin/tool", "bin/link", "lib", "lib/deep", "lib/deep/f"}},
		"com.example.b": {plist: pkgInfoPlist(volume, "opt/y")},
	}, "com.example.a\ncom.example.b\n")}
	ti.in.Run = f.run
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	sudo := func(name string, args ...string) []string {
		return append([]string{"-u", "root", "-E", "--", name}, args...)
	}
	rm := func(input ...string) Command {
		return Command{Name: "/usr/bin/sudo", Args: sudo("/usr/bin/xargs", "-0", "--", "/bin/rm", "--"), Stdin: []byte(strings.Join(input, "\x00"))}
	}
	rmdir := func(input ...string) Command {
		return Command{Name: "/usr/bin/sudo", Args: sudo("/usr/bin/xargs", "-0", "--", "/bin/bash", "-c", rmdirScript, "rmdir.sh"), Stdin: []byte(strings.Join(input, "\x00"))}
	}
	forget := func(id string) Command {
		return Command{Name: "/usr/bin/sudo", Args: sudo("/usr/sbin/pkgutil", "--forget", id)}
	}
	env := func(args ...string) Command {
		return Command{Name: "/usr/bin/env", Args: append([]string{"/usr/sbin/pkgutil"}, args...)}
	}
	want := []Command{
		env("--pkgs=com.example.*"),
		env("--files", "com.example.a"),
		env("--pkg-info-plist", "com.example.a"),
		rm(filepath.Join(root, "bin/tool"), filepath.Join(root, "lib/deep/f")),
		rm(filepath.Join(root, "bin/link")),
		rmdir(filepath.Join(root, "lib/deep"), filepath.Join(root, "bin"), filepath.Join(root, "lib")),
		rmdir(root),
		forget("com.example.a"),
		env("--files", "com.example.b"),
		env("--pkg-info-plist", "com.example.b"),
		rmdir(filepath.Join(volume, "opt/y")),
		forget("com.example.b"),
	}
	var got []Command
	for _, c := range f.calls {
		got = append(got, Command{Name: c.Name, Args: c.Args, Stdin: c.Stdin})
	}
	if len(got) != len(want) {
		t.Fatalf("calls = %d, want %d:\n%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Name != want[i].Name || !slices.Equal(got[i].Args, want[i].Args) || string(got[i].Stdin) != string(want[i].Stdin) {
			t.Errorf("call %d = %+v\nwant %+v", i, got[i], want[i])
		}
	}
	wantOut := "==> Uninstalling packages with `sudo` (which may request your password)...\ncom.example.a\ncom.example.b\n"
	if !strings.Contains(ti.out.String(), wantOut) {
		t.Errorf("out = %q, want it to contain %q", ti.out.String(), wantOut)
	}
}

func TestUninstallPkgutilLeavesUndeletablePaths(t *testing.T) {
	ti, _ := installed(t, `[{"app":["App.app"]}]`)
	receipt := filepath.Join(ti.prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json")
	if err := os.WriteFile(receipt, []byte(`{"uninstall_artifacts":[{"uninstall":[{"pkgutil":"com.example.a"}]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeRun{handle: fakePkgutil(map[string]bomPkg{
		"com.example.a": {plist: pkgInfoPlist("/", "usr"), files: []string{"local", "local/bin", "local/bin/zzz-brw-test-missing"}},
	}, "com.example.a\n")}
	ti.in.Run = f.run
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	calls := f.sudoCalls()
	if len(calls) != 1 || calls[0].Args[len(calls[0].Args)-2] != "--forget" {
		t.Errorf("only the receipt should be forgotten, got %+v", calls)
	}
}

func TestUninstallPkgutilWithNoMatchRunsNothingElse(t *testing.T) {
	ti, _ := installed(t, `[{"app":["App.app"]}]`)
	receipt := filepath.Join(ti.prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json")
	if err := os.WriteFile(receipt, []byte(`{"uninstall_artifacts":[{"uninstall":[{"pkgutil":"com.none"}]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeRun{handle: func(Command) ([]byte, error) { return nil, errors.New("exit status 1") }}
	ti.in.Run = f.run
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0].Args[1] != "--pkgs=com.none" {
		t.Errorf("calls = %+v", f.calls)
	}
}

func TestUninstallPkgutilFailureStops(t *testing.T) {
	ti, _ := installed(t, `[{"app":["App.app"]}]`)
	receipt := filepath.Join(ti.prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json")
	if err := os.WriteFile(receipt, []byte(`{"uninstall_artifacts":[{"uninstall":[{"pkgutil":"com.example.a"}]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeRun{handle: fakePkgutil(nil, "com.example.a\n")}
	ti.in.Run = f.run
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err == nil {
		t.Error("a failing pkgutil --files should fail the uninstall")
	}
	if len(f.sudoCalls()) > 0 {
		t.Errorf("ran %+v", f.sudoCalls())
	}
}

func TestUninstallPkgutilRefusesToRunAsRoot(t *testing.T) {
	ti, _ := installed(t, `[{"app":["App.app"]}]`)
	receipt := filepath.Join(ti.prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json")
	if err := os.WriteFile(receipt, []byte(`{"uninstall_artifacts":[{"uninstall":[{"pkgutil":"com.example.a"}]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	asRoot(t)
	f := &fakeRun{}
	ti.in.Run = f.run
	err := ti.in.Uninstall(context.Background(), "app", "1.0", false)
	if err == nil || !strings.Contains(err.Error(), "Running Homebrew as root is extremely dangerous") || len(f.calls) > 0 {
		t.Errorf("err = %v, calls = %+v", err, f.calls)
	}
}

func TestUninstallPkgOnlyCaskWithoutAnUninstallStanza(t *testing.T) {
	ti := newTestInstall(t, pkgTarball)
	ti.in.Run = (&fakeRun{}).run
	if err := ti.in.Install(context.Background(), ti.cask(t, `[{"pkg":["Sub/P.pkg"]}]`)); err != nil {
		t.Fatal(err)
	}
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(ti.prefix, "Caskroom/app/1.0")) {
		t.Error("the staged files should be purged")
	}
}

func TestPlistDumpMatchesRuby(t *testing.T) {
	got, err := plistDump([]byte(`[{"choiceIdentifier":"a&<b\"'","attributeSetting":1,"x":1.5,"t":true,"f":false,"e":[],"h":{},"n":{"z":"y"},"nil":null,"empty":""}]`))
	if err != nil {
		t.Fatal(err)
	}
	want := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
		"<plist version=\"1.0\">\n<array>\n" +
		"\t<dict>\n" +
		"\t\t<key>attributeSetting</key>\n\t\t<integer>1</integer>\n" +
		"\t\t<key>choiceIdentifier</key>\n\t\t<string>a&amp;&lt;b&quot;&#39;</string>\n" +
		"\t\t<key>e</key>\n\t\t<array/>\n" +
		"\t\t<key>empty</key>\n\t\t<string/>\n" +
		"\t\t<key>f</key>\n\t\t<false/>\n" +
		"\t\t<key>h</key>\n\t\t<dict/>\n" +
		"\t\t<key>n</key>\n\t\t<dict>\n\t\t\t<key>z</key>\n\t\t\t<string>y</string>\n\t\t</dict>\n" +
		"\t\t<key>nil</key>\n" +
		"<!-- The <data> element below contains a Ruby object which has been serialized with Marshal.dump. -->\n" +
		"\t\t<data>\n\t\tBAgw\n\t\t</data>\n" +
		"\t\t<key>t</key>\n\t\t<true/>\n" +
		"\t\t<key>x</key>\n\t\t<real>1.5</real>\n" +
		"\t</dict>\n</array>\n</plist>\n"
	if string(got) != want {
		t.Errorf("plist =\n%s\nwant\n%s", got, want)
	}
}

func TestPlistDumpOfAScalar(t *testing.T) {
	got, err := plistDump([]byte(`"s"`))
	if err != nil {
		t.Fatal(err)
	}
	if want := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\">\n<string>s</string>\n</plist>\n"; string(got) != want {
		t.Errorf("plist = %q", got)
	}
}

func TestUndeletableMatchesContainersAndResolvedParents(t *testing.T) {
	home := "/Users/me"
	for path, want := range map[string]bool{
		"/Users/me/Library/Keychains/login.keychain-db":         true,
		"/Users/me/Library/Application Support/com.apple.TCC/x": true,
		"/Users/me/Library/Messages/chat.db":                    true,
		"/Users/me/Library/Application Support/Other/x":         false,
		"/tmp":                            true,
		"/private/tmp":                    true,
		"/usr/local/zzz-brw-test-missing": false,
		"/Applications":                   true,
		"/Applications/Foo.app":           false,
	} {
		if got := undeletable(path, home); got != want {
			t.Errorf("undeletable(%q) = %v, want %v", path, got, want)
		}
	}
}
