package cask

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type movedCase struct {
	kind, english, article string
	dir                    DirKey
}

var movedCases = []movedCase{
	{"suite", "App Suite", "an", DirApp},
	{"colorpicker", "Colorpicker", "a", DirColorPicker},
	{"prefpane", "Preference Pane", "a", DirPrefPane},
	{"qlplugin", "Quick Look Plugin", "a", DirQLPlugin},
	{"mdimporter", "Spotlight metadata importer", "a", DirMDImporter},
	{"dictionary", "Dictionary", "a", DirDictionary},
	{"font", "Font", "a", DirFont},
	{"service", "Service", "a", DirService},
	{"input_method", "Input Method", "an", DirInputMethod},
	{"internet_plugin", "Internet Plugin", "an", DirInternetPlug},
	{"audio_unit_plugin", "Audio Unit Plugin", "an", DirAudioUnitPlug},
	{"vst_plugin", "VST Plugin", "a", DirVSTPlug},
	{"vst3_plugin", "VST3 Plugin", "a", DirVST3Plug},
	{"screen_saver", "Screen Saver", "a", DirScreenSaver},
}

func movedInstall(t *testing.T, c movedCase) (*testInstall, *fakeExec, string) {
	t.Helper()
	ti := newTestInstall(t, appTarball)
	f := &fakeExec{running: map[string]int{}, fail: map[string]bool{}, gui: true}
	ti.in.Exec, ti.in.Run, ti.in.FindService = f.exec, f.run, f.findService
	dir := t.TempDir()
	if c.dir == DirApp {
		ti.in.Where.AppDir = dir
	} else {
		ti.in.Where.Dirs[c.dir] = dir
	}
	return ti, f, filepath.Join(dir, "App.app")
}

func (c movedCase) artifacts() string { return `[{"` + c.kind + `":["App.app"]}]` }

func TestMovedKindsMoveIntoTheirDirAndBack(t *testing.T) {
	for _, c := range movedCases {
		t.Run(c.kind, func(t *testing.T) {
			ti, f, target := movedInstall(t, c)
			if err := ti.in.Install(context.Background(), ti.cask(t, c.artifacts())); err != nil {
				t.Fatal(err)
			}
			staged := filepath.Join(ti.prefix, "Caskroom/app/1.0/App.app")
			if !exists(filepath.Join(target, "Contents/Info.plist")) {
				t.Errorf("%s was not moved to %s", c.english, target)
			}
			if got := readLink(t, staged); got != target {
				t.Errorf("staged link -> %s, want %s", got, target)
			}
			if want := "==> Moving " + c.english + " 'App.app' to '" + target + "'\n"; !strings.Contains(ti.out.String(), want) {
				t.Errorf("output = %q, want it to contain %q", ti.out.String(), want)
			}
			wantQL, wantMD := 1, 0
			if c.kind == "mdimporter" {
				wantQL, wantMD = 0, 1
			}
			if c.kind != "qlplugin" && c.kind != "mdimporter" {
				wantQL = 0
			}
			if got := len(f.ran(qlmanageTool + " -r")); got != wantQL {
				t.Errorf("qlmanage ran %d times on install, want %d", got, wantQL)
			}
			if got := f.ran(mdimportTool + " -r"); len(got) != wantMD || wantMD == 1 && got[0] != mdimportTool+" -r "+target {
				t.Errorf("mdimport calls = %q", got)
			}
			if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
				t.Fatal(err)
			}
			if exists(target) {
				t.Errorf("uninstall left the %s in place", c.english)
			}
			if c.kind == "qlplugin" && len(f.ran(qlmanageTool+" -r")) != 2 {
				t.Errorf("qlmanage calls = %q, want one for install and one for uninstall", f.ran(qlmanageTool))
			}
			if !strings.Contains(ti.out.String(), "==> Backing up "+c.english+" 'App.app' to '"+staged+"'\n==> Removing "+c.english+" '"+target+"'\n") {
				t.Errorf("uninstall output = %q", ti.out.String())
			}
		})
	}
}

func TestMovedKindsRefuseAnOccupiedTarget(t *testing.T) {
	for _, c := range movedCases {
		t.Run(c.kind, func(t *testing.T) {
			ti, _, target := movedInstall(t, c)
			if err := os.MkdirAll(filepath.Join(target, "old"), 0o755); err != nil {
				t.Fatal(err)
			}
			message := "It seems there is already " + c.article + " " + c.english + " at '" + target + "'"
			err := ti.in.Install(context.Background(), ti.cask(t, c.artifacts()))
			if err == nil || err.Error() != "app: "+message+"." {
				t.Fatalf("err = %v, want %q", err, message+".")
			}
			if !exists(filepath.Join(target, "old")) {
				t.Error("a refused install touched the existing target")
			}
			if exists(filepath.Join(ti.prefix, "Caskroom/app")) {
				t.Error("a refused install left Caskroom/app behind")
			}
		})
	}
}

func TestMovedAdoptKeepsTheExistingTargetEvenWhenALaterArtifactFails(t *testing.T) {
	c := movedCases[6]
	ti, _, target := movedInstall(t, c)
	ti.in.Adopt = true
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "mine"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ti.in.Where.Dirs[DirPrefPane] = t.TempDir()
	err := ti.in.Install(context.Background(), ti.cask(t, `[{"font":["App.app"]},{"prefpane":["Missing.prefPane"]}]`))
	if err == nil {
		t.Fatal("expected the missing prefpane source to fail")
	}
	if want := "==> Adopting existing Font at '" + target + "'\n"; !strings.Contains(ti.out.String(), want) {
		t.Errorf("output = %q, want it to contain %q", ti.out.String(), want)
	}
	if !exists(filepath.Join(target, "mine")) {
		t.Error("a rollback must not remove an adopted target")
	}
}

func TestMovedFailureRemovesEarlierArtifacts(t *testing.T) {
	ti, _, target := movedInstall(t, movedCases[6])
	ti.in.Where.Dirs[DirPrefPane] = t.TempDir()
	c := ti.cask(t, `[{"font":["App.app"]},{"prefpane":["Missing.prefPane"]}]`)
	err := ti.in.Install(context.Background(), c)
	want := "app: It seems the Preference Pane source '" + filepath.Join(ti.prefix, "Caskroom/app/1.0/Missing.prefPane") + "' is not there."
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %s", err, want)
	}
	if exists(target) {
		t.Error("the font should have been removed again")
	}
	if exists(filepath.Join(ti.prefix, "Caskroom/app")) {
		t.Error("Caskroom/app left behind")
	}
}

func TestGenericArtifactNeedsATarget(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	err := ti.in.Install(context.Background(), ti.cask(t, `[{"artifact":["App.app"]}]`))
	want := "Cask 'app' definition is invalid: Generic Artifact 'App.app' requires a target."
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %s", err, want)
	}
	target := filepath.Join(t.TempDir(), "Generic/App.app")
	if err := ti.in.Install(context.Background(), ti.cask(t, `[{"artifact":["App.app",{"target":"`+target+`"}]}]`)); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(target, "Contents/Info.plist")) {
		t.Error("the generic artifact was not moved to its target")
	}
}

func TestArtifactPrefixAndTargetResolution(t *testing.T) {
	w := NewWhere("/p", "/Apps", "/Users/me")
	w.Dirs[DirFont] = "/Fonts"
	for _, tt := range []struct{ args, want string }{
		{`["a/B.ttf"]`, "/Fonts/B.ttf"},
		{`["B.ttf",{"target":"C.ttf"}]`, "/Fonts/C.ttf"},
		{`["B.ttf",{"target":"~/Library/Other/C.ttf"}]`, "/Users/me/Library/Other/C.ttf"},
		{`["B.ttf",{"target":"/abs/C.ttf"}]`, "/abs/C.ttf"},
	} {
		m, err := readMove([]byte(tt.args), "font", w)
		if err != nil || m.target != tt.want {
			t.Errorf("%s: target = %q, %v; want %s", tt.args, m.target, err, tt.want)
		}
	}
}

func TestReadMoveResolvesRelativeTargetsPerKind(t *testing.T) {
	w := NewWhere("/p", "/Apps", "/Users/me")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ kind, args, want string }{
		{"app", `["sub/B.app"]`, "/Apps/B.app"},
		{"suite", `["B",{"target":"Dir/B"}]`, "/Apps/Dir/B"},
		{"binary", `["tool"]`, "/p/bin/tool"},
		{"binary", `["tool",{"target":"x"}]`, "/p/bin/x"},
		{"artifact", `["B",{"target":"/abs/B"}]`, "/abs/B"},
		{"artifact", `["B",{"target":"rel/B"}]`, filepath.Join(cwd, "rel/B")},
		{"artifact", `["B",{"target":"~/B"}]`, "/Users/me/B"},
		{"vst3_plugin", `["B.vst3",{"target":""}]`, "/Users/me/Library/Audio/Plug-Ins/VST3/B.vst3"},
	} {
		m, err := readMove([]byte(tt.args), tt.kind, w)
		if err != nil || m.target != tt.want {
			t.Errorf("%s %s: target = %q, %v; want %s", tt.kind, tt.args, m.target, err, tt.want)
		}
	}
	if _, err := readMove([]byte(`["B"]`), "artifact", w); !errors.Is(err, errNoTarget) {
		t.Errorf("artifact without a target: err = %v", err)
	}
	if _, err := readMove([]byte(`["B",{"target":""}]`), "artifact", w); err != nil {
		t.Errorf("artifact with an empty target string counts as given: %v", err)
	}
}

func TestSystemDirDecidesWhetherAppsAreMadeWorldReadable(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	ti.in.Where.Home = "/Users/me"
	for target, want := range map[string]bool{
		"/Applications/Foo.app":              true,
		"/Library/Foo.app":                   true,
		"/Users/me/Applications/Foo.app":     false,
		"/Users/me/Library/Foo.app":          false,
		"/opt/stuff/Foo.app":                 true,
		"/Volumes/Data/Apps/Foo.app":         true,
		"/Volumes/Data/Apps/Deeper/Foo.app":  true,
		"/Somewhere/else/Deeper/Foo.app":     false,
		"/Users/me/Applications/Sub/Foo.app": false,
	} {
		if got := ti.in.systemDir(target); got != want {
			t.Errorf("systemDir(%s) = %v, want %v", target, got, want)
		}
	}
}

func TestPlanKeepsEveryMovedKindInStanzaOrder(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	p, err := plan(ti.cask(t, `[{"font":["a.ttf"]},{"app":["A.app"]},{"vst3_plugin":["v.vst3"]}]`), ti.in.Where)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, m := range p.moved {
		kinds = append(kinds, m.kind)
	}
	if want := []string{"font", "app", "vst3_plugin"}; !slices.Equal(kinds, want) {
		t.Errorf("kinds = %v, want %v", kinds, want)
	}
}
