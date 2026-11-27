package cask

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigJSONRecordsExplicitAndEnvDirsInBrewsOrder(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	ti.in.Config = Config{
		Languages: []string{"en-ZA"},
		Explicit:  map[DirKey]string{DirFont: "/F", DirApp: "/A", DirKeyboard: "/K"},
		Env:       map[DirKey]string{DirVST3Plug: "/V3"},
	}
	if err := ti.in.Install(context.Background(), ti.cask(t, `[{"app":["App.app"]}]`)); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, filepath.Join(ti.prefix, "Caskroom/app/.metadata/config.json"))
	for _, want := range []string{
		`"env":{"vst3_plugindir":"/V3"}`,
		`"explicit":{"appdir":"/A","keyboard_layoutdir":"/K","fontdir":"/F"}}`,
		`{"default":{"languages":["en-ZA"],"appdir":"/Applications","appimagedir":"/Users/me/Applications","keyboard_layoutdir":"/Library/Keyboard Layouts"`,
		`"screen_saverdir":"/Users/me/Library/Screen Savers"},`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config.json = %s\nwant it to contain %s", got, want)
		}
	}
}

func TestDirsFromOptionsReadsHyphenatedFlagsLikeBrewsEnvConfig(t *testing.T) {
	got := DirsFromOptions([]string{"--fontdir=~/Fonts", "--input-methoddir=/IM", "--language=en", "--bogusdir=/x", "--appdir"}, "/Users/me")
	want := map[DirKey]string{DirFont: "/Users/me/Fonts", DirInputMethod: "/IM"}
	if len(got) != len(want) || got[DirFont] != want[DirFont] || got[DirInputMethod] != want[DirInputMethod] {
		t.Errorf("dirs = %v, want %v", got, want)
	}
}

func TestWhereWithConfigPrefersExplicitThenEnvThenDefault(t *testing.T) {
	w := NewWhere("/p", "/Applications", "/Users/me").WithConfig(Config{
		Explicit: map[DirKey]string{DirFont: "/X"},
		Env:      map[DirKey]string{DirFont: "/E", DirPrefPane: "/E2"},
	})
	for key, want := range map[DirKey]string{
		DirFont: "/X", DirPrefPane: "/E2", DirApp: "/Applications", DirScreenSaver: "/Users/me/Library/Screen Savers",
	} {
		if got := w.Dir(key); got != want {
			t.Errorf("Dir(%s) = %s, want %s", key, got, want)
		}
	}
}
