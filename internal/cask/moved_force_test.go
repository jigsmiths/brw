package cask

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMovedKindsOverwriteAnOccupiedTargetWhenForced(t *testing.T) {
	for _, c := range movedCases {
		t.Run(c.kind, func(t *testing.T) {
			ti, _, target := movedInstall(t, c)
			if err := os.MkdirAll(filepath.Join(target, "old"), 0o755); err != nil {
				t.Fatal(err)
			}
			message := "It seems there is already " + c.article + " " + c.english + " at '" + target + "'"
			ti.in.Force = true
			if err := ti.in.Install(context.Background(), ti.cask(t, c.artifacts())); err != nil {
				t.Fatal(err)
			}
			if want := "Warning: " + message + "; overwriting.\n"; ti.warn.String() != want {
				t.Errorf("warnings = %q, want %q", ti.warn.String(), want)
			}
			if want := "==> Removing " + c.english + " '" + target + "'\n==> Moving " + c.english; !strings.Contains(ti.out.String(), want) {
				t.Errorf("output = %q, want it to contain %q", ti.out.String(), want)
			}
			if exists(filepath.Join(target, "old")) || !exists(filepath.Join(target, "Contents/Info.plist")) {
				t.Error("forcing should replace the target with the staged one")
			}
		})
	}
}

func TestMovedFailureUndoesEarlierArtifactsAsUninstallDoes(t *testing.T) {
	ti, _, target := movedInstall(t, movedCases[6])
	ti.in.Where.Dirs[DirPrefPane] = t.TempDir()
	c := ti.cask(t, `[{"font":["App.app"]},{"prefpane":["Missing.prefPane"]}]`)
	err := ti.in.Install(context.Background(), c)
	want := "app: It seems the Preference Pane source '" + filepath.Join(ti.prefix, "Caskroom/app/1.0/Missing.prefPane") + "' is not there."
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %s", err, want)
	}
	if exists(target) {
		t.Error("the font should have been backed up and removed again")
	}
	if want := "==> Backing up Font 'App.app' to '"; !strings.Contains(ti.out.String(), want) {
		t.Errorf("output = %q, want the rollback to back the font up", ti.out.String())
	}
	if exists(filepath.Join(ti.prefix, "Caskroom/app")) {
		t.Error("Caskroom/app left behind")
	}
}

func TestMovedUpgradeKeepsAnEmptiedTargetDirAndFillsIt(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "App.app")
	artifacts := `[{"font":["App.app",{"target":"` + target + `"}]}]`
	ti, _, c := upgradable(t, artifacts)
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := ti.in.Upgrade(context.Background(), installedCask(t, ti.prefix), c); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(target)
	if err != nil || !os.SameFile(before, after) {
		t.Errorf("the target directory was replaced instead of emptied and refilled: %v", err)
	}
	if got := readFile(t, filepath.Join(target, "Contents/Info.plist")); got != "<plist>2</plist>" {
		t.Errorf("Info.plist = %q", got)
	}
	if got := readLink(t, filepath.Join(ti.prefix, "Caskroom/app/2.0/App.app")); got != target {
		t.Errorf("staged link -> %s", got)
	}
}

func TestMovedUpgradeNeedsTheInstalledTargetUnlessForced(t *testing.T) {
	target := filepath.Join(t.TempDir(), "App.app")
	artifacts := `[{"font":["App.app",{"target":"` + target + `"}]}]`
	ti, _, c := upgradable(t, artifacts)
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	err := ti.in.Upgrade(context.Background(), installedCask(t, ti.prefix), c)
	want := "app: It seems the Font source '" + target + "' is not there."
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %s", err, want)
	}
}
