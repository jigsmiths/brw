package cask

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
)

func TestPlanLinkArtifacts(t *testing.T) {
	for _, tc := range []struct {
		name, artifact, wantTarget string
	}{
		{"manpage section 1", `{"manpage":["man/app.1"]}`, "/opt/homebrew/share/man/man1/app.1"},
		{"manpage gz", `{"manpage":["man/app.1.gz"]}`, "/opt/homebrew/share/man/man1/app.1.gz"},
		{"manpage section n", `{"manpage":["man/app.n"]}`, "/opt/homebrew/share/man/mann/app.n"},
		{"manpage section l", `{"manpage":["man/app.l.gz"]}`, "/opt/homebrew/share/man/manl/app.l.gz"},
		{"manpage target", `{"manpage":["man/app.1",{"target":"other.1"}]}`, "/opt/homebrew/share/man/man1/other.1"},
		{"bash", `{"bash_completion":["c/app.bash"]}`, "/opt/homebrew/etc/bash_completion.d/app"},
		{"bash target", `{"bash_completion":["c/app.bash",{"target":"x.sh"}]}`, "/opt/homebrew/etc/bash_completion.d/x"},
		{"zsh", `{"zsh_completion":["c/app.zsh"]}`, "/opt/homebrew/share/zsh/site-functions/_app"},
		{"zsh underscore", `{"zsh_completion":["c/_app"]}`, "/opt/homebrew/share/zsh/site-functions/_app"},
		{"zsh target", `{"zsh_completion":["c/app.zsh",{"target":"_renamed"}]}`, "/opt/homebrew/share/zsh/site-functions/_renamed"},
		{"fish", `{"fish_completion":["c/app"]}`, "/opt/homebrew/share/fish/vendor_completions.d/app.fish"},
		{"fish suffix", `{"fish_completion":["c/app.fish"]}`, "/opt/homebrew/share/fish/vendor_completions.d/app.fish"},
		{"fish target", `{"fish_completion":["c/app.fish",{"target":"renamed"}]}`, "/opt/homebrew/share/fish/vendor_completions.d/renamed.fish"},
		{"manpage absolute target", `{"manpage":["man/app.1",{"target":"/opt/homebrew/share/man/man1/x.1"}]}`, "/opt/homebrew/share/man/man1/x.1"},
		{"bash absolute target", `{"bash_completion":["c/app.bash",{"target":"/opt/homebrew/etc/bash_completion.d/aerospace"}]}`, "/opt/homebrew/etc/bash_completion.d/aerospace"},
		{"zsh absolute target", `{"zsh_completion":["c/app.zsh",{"target":"/opt/homebrew/share/zsh/site-functions/_aerospace"}]}`, "/opt/homebrew/share/zsh/site-functions/_aerospace"},
		{"fish absolute target", `{"fish_completion":["c/app.fish",{"target":"/opt/homebrew/share/fish/vendor_completions.d/aerospace.fish"}]}`, "/opt/homebrew/share/fish/vendor_completions.d/aerospace.fish"},
		{"fish absolute target without suffix", `{"fish_completion":["c/app",{"target":"/elsewhere/x"}]}`, "/opt/homebrew/share/fish/vendor_completions.d/x.fish"},
		{"bash dotfile name", `{"bash_completion":["c/.app"]}`, "/opt/homebrew/etc/bash_completion.d/.app"},
	} {
		p, err := plan(api.Cask{Token: "x", Artifacts: raws(t, `[`+tc.artifact+`]`)}, where)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(p.links) != 1 || p.links[0].target != tc.wantTarget {
			t.Errorf("%s: links = %+v, want target %s", tc.name, p.links, tc.wantTarget)
			continue
		}
		if kind := p.links[0].kind; !strings.HasPrefix(tc.artifact, `{"`+kind+`"`) {
			t.Errorf("%s: kind = %q", tc.name, kind)
		}
	}
}

func TestPlanRefusesAnInvalidManpageName(t *testing.T) {
	c := api.Cask{Token: "x", Artifacts: raws(t, `[{"manpage":["man/app.9"]}]`)}
	_, err := plan(c, where)
	if want := "Cask 'x' definition is invalid: 'man/app.9' is not a valid man page name"; err == nil || err.Error() != want {
		t.Errorf("err = %v", err)
	}
}

func TestPlanKeepsLinkArtifactsInTheReceiptAsGiven(t *testing.T) {
	c := api.Cask{Token: "x", Artifacts: raws(t, `[{"binary":["tool"]},{"manpage":["man/app.1"]},{"zsh_completion":["c/app.zsh"]}]`)}
	p, err := plan(c, where)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := json.Marshal(p.uninstall); string(got) != `[{"binary":["tool"]},{"manpage":["man/app.1"]},{"zsh_completion":["c/app.zsh"]}]` {
		t.Errorf("uninstall artifacts = %s", got)
	}
}

var linksTarball = func() []byte {
	var t testing.T
	return tarGzBytes(&t,
		entry{name: "tool", body: "#!/bin/sh\n"},
		entry{name: "man/app.1", body: "man"},
		entry{name: "c/app.bash", body: "bash"},
		entry{name: "c/app.zsh", body: "zsh"},
		entry{name: "c/app.fish", body: "fish"})
}()

const linkArtifacts = `[
	{"binary":["tool"]},
	{"manpage":["man/app.1"]},
	{"bash_completion":["c/app.bash"]},
	{"zsh_completion":["c/app.zsh"]},
	{"fish_completion":["c/app.fish"]}]`

type linkPaths struct{ man, bash, zsh, fish string }

func linkTargets(prefix string) linkPaths {
	return linkPaths{
		man:  filepath.Join(prefix, "share/man/man1/app.1"),
		bash: filepath.Join(prefix, "etc/bash_completion.d/app"),
		zsh:  filepath.Join(prefix, "share/zsh/site-functions/_app"),
		fish: filepath.Join(prefix, "share/fish/vendor_completions.d/app.fish"),
	}
}

func installedLinks(t *testing.T) *testInstall {
	t.Helper()
	ti := newTestInstall(t, linksTarball)
	if err := ti.in.Install(context.Background(), ti.cask(t, linkArtifacts)); err != nil {
		t.Fatal(err)
	}
	return ti
}

func TestInstallLinksManpageAndCompletions(t *testing.T) {
	ti := installedLinks(t)
	version := filepath.Join(ti.prefix, "Caskroom/app/1.0")
	l := linkTargets(ti.prefix)
	for target, source := range map[string]string{
		l.man: "man/app.1", l.bash: "c/app.bash", l.zsh: "c/app.zsh", l.fish: "c/app.fish",
	} {
		if got := readLink(t, target); got != filepath.Join(version, source) {
			t.Errorf("%s -> %s, want %s", target, got, source)
		}
	}
	if fi, err := os.Stat(filepath.Join(version, "man/app.1")); err != nil || fi.Mode().Perm()&0o111 != 0 {
		t.Errorf("a manpage must not be made executable: %v %v", fi, err)
	}
	want := "==> Installing Cask app\n" +
		"==> Linking Binary 'tool' to '" + filepath.Join(ti.prefix, "bin/tool") + "'\n" +
		"==> Linking Manpage 'app.1' to '" + l.man + "'\n" +
		"==> Linking Bash Completion 'app.bash' to '" + l.bash + "'\n" +
		"==> Linking Zsh Completion 'app.zsh' to '" + l.zsh + "'\n" +
		"==> Linking Fish Completion 'app.fish' to '" + l.fish + "'\n" +
		"🍺  app was successfully installed!\n"
	if got := ti.out.String(); got != want {
		t.Errorf("output =\n%s\nwant\n%s", got, want)
	}
	receipt := readFile(t, filepath.Join(ti.prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json"))
	for _, frag := range []string{
		"{\n      \"manpage\": [\n        \"man/app.1\"\n      ]\n    }",
		"{\n      \"bash_completion\": [\n        \"c/app.bash\"\n      ]\n    }",
		"{\n      \"zsh_completion\": [\n        \"c/app.zsh\"\n      ]\n    }",
		"{\n      \"fish_completion\": [\n        \"c/app.fish\"\n      ]\n    }",
	} {
		if !strings.Contains(receipt, frag) {
			t.Errorf("receipt lacks %s:\n%s", frag, receipt)
		}
	}
}

func TestInstallRefusesAnExistingManpage(t *testing.T) {
	ti := newTestInstall(t, linksTarball)
	l := linkTargets(ti.prefix)
	writeFile(t, l.man, []byte("mine"), 0o644)
	err := ti.in.Install(context.Background(), ti.cask(t, linkArtifacts))
	if want := "app: It seems there is already a Manpage at '" + l.man + "'."; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
	if ti.hits.Load() != 0 {
		t.Error("downloaded before checking the targets")
	}
}

func TestInstallNotesAnAlreadyLinkedCompletion(t *testing.T) {
	ti := newTestInstall(t, linksTarball)
	l := linkTargets(ti.prefix)
	if err := os.MkdirAll(filepath.Dir(l.zsh), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ti.prefix, "Caskroom/app/1.0/c/app.zsh"), l.zsh); err != nil {
		t.Fatal(err)
	}
	if err := ti.in.Install(context.Background(), ti.cask(t, linkArtifacts)); err != nil {
		t.Fatal(err)
	}
	if want := "==> Zsh Completion 'app.zsh' is already linked to '" + l.zsh + "'\n"; !strings.Contains(ti.out.String(), want) {
		t.Errorf("output:\n%s", ti.out)
	}
}

func TestInstallUndoesLinksOfAFailedInstall(t *testing.T) {
	ti := newTestInstall(t, linksTarball)
	c := ti.cask(t, `[{"binary":["tool"]},{"manpage":["man/app.1"]},{"fish_completion":["c/missing.fish"]}]`)
	if err := ti.in.Install(context.Background(), c); err == nil {
		t.Fatal("expected an error")
	}
	l := linkTargets(ti.prefix)
	for _, p := range []string{l.man, filepath.Join(ti.prefix, "bin/tool"), filepath.Join(ti.prefix, "Caskroom/app")} {
		if exists(p) {
			t.Errorf("%s left behind", p)
		}
	}
}

func TestUninstallUnlinksManpageAndCompletions(t *testing.T) {
	ti := installedLinks(t)
	ti.out.Reset()
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	l := linkTargets(ti.prefix)
	for _, p := range []string{l.man, l.bash, l.zsh, l.fish, filepath.Join(ti.prefix, "bin/tool")} {
		if exists(p) {
			t.Errorf("%s is still there", p)
		}
	}
	want := "==> Uninstalling Cask app\n" +
		"==> Unlinking Binary '" + filepath.Join(ti.prefix, "bin/tool") + "'\n" +
		"==> Unlinking Manpage '" + l.man + "'\n" +
		"==> Unlinking Bash Completion '" + l.bash + "'\n" +
		"==> Unlinking Zsh Completion '" + l.zsh + "'\n" +
		"==> Unlinking Fish Completion '" + l.fish + "'\n" +
		"==> Purging files for version 1.0 of Cask app\n"
	if got := ti.out.String(); got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
}

func TestUpgradeRelinksManpageAndCompletions(t *testing.T) {
	ti := installedLinks(t)
	srv, _ := server(t, linksTarball, nil)
	c := api.Cask{
		Token: "app", Tap: "homebrew/cask", TapGitHead: "abc123", Version: "2.0",
		URL: srv.URL + "/App-2.0.tar.gz", SHA256: sum(linksTarball), Artifacts: raws(t, linkArtifacts),
	}
	ti.out.Reset()
	if err := ti.in.Upgrade(context.Background(), installedCask(t, ti.prefix), c); err != nil {
		t.Fatal(err)
	}
	version := filepath.Join(ti.prefix, "Caskroom/app/2.0")
	l := linkTargets(ti.prefix)
	for target, source := range map[string]string{
		l.man: "man/app.1", l.bash: "c/app.bash", l.zsh: "c/app.zsh", l.fish: "c/app.fish",
	} {
		if got := readLink(t, target); got != filepath.Join(version, source) {
			t.Errorf("%s -> %s, want %s", target, got, source)
		}
	}
	for _, want := range []string{
		"==> Unlinking Manpage '" + l.man + "'\n",
		"==> Linking Manpage 'app.1' to '" + l.man + "'\n",
		"==> Linking Fish Completion 'app.fish' to '" + l.fish + "'\n",
	} {
		if !strings.Contains(ti.out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, ti.out)
		}
	}
}

func TestUpgradeRestoresLinksOfAFailedUpgrade(t *testing.T) {
	ti := installedLinks(t)
	srv, _ := server(t, linksTarball, nil)
	c := api.Cask{
		Token: "app", Tap: "homebrew/cask", TapGitHead: "abc123", Version: "2.0",
		URL: srv.URL + "/App-2.0.tar.gz", SHA256: sum(linksTarball),
		Artifacts: raws(t, `[{"binary":["tool"]},{"manpage":["man/app.1"]},{"bash_completion":["c/missing.bash"]}]`),
	}
	if err := ti.in.Upgrade(context.Background(), installedCask(t, ti.prefix), c); err == nil {
		t.Fatal("expected an error")
	}
	version := filepath.Join(ti.prefix, "Caskroom/app/1.0")
	l := linkTargets(ti.prefix)
	for target, source := range map[string]string{
		l.man: "man/app.1", l.bash: "c/app.bash", l.zsh: "c/app.zsh", l.fish: "c/app.fish",
	} {
		if got := readLink(t, target); got != filepath.Join(version, source) {
			t.Errorf("%s -> %s, want %s", target, got, source)
		}
	}
}
