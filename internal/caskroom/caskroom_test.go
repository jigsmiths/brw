package caskroom

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// install records an install of a cask the way brew does, storing its
// definition with the given extension, or none when ext is empty.
func install(t *testing.T, prefix, token, version, stamp, ext string) {
	t.Helper()
	casks := filepath.Join(prefix, "Caskroom", token, ".metadata", version, stamp, "Casks")
	if err := os.MkdirAll(casks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "Caskroom", token, version), 0o755); err != nil {
		t.Fatal(err)
	}
	if ext != "" {
		if err := os.WriteFile(filepath.Join(casks, token+ext), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Files beside the version directories, as brew writes them.
	for _, name := range []string{"INSTALL_RECEIPT.json", "config.json"} {
		if err := os.WriteFile(filepath.Join(prefix, "Caskroom", token, ".metadata", name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func tokens(casks []Cask) []string {
	var out []string
	for _, c := range casks {
		out = append(out, c.Token+" "+c.Version)
	}
	return out
}

func TestInstalled(t *testing.T) {
	prefix := t.TempDir()
	room := filepath.Join(prefix, "Caskroom")
	install(t, prefix, "slack", "4.53.292", "20261117083153.956", ".json")
	install(t, prefix, "old", "1.0", "20200101000000.000", ".rb")
	// Upgraded: the latest timestamp wins, whatever the versions say.
	install(t, prefix, "gcloud-cli", "594.0.0", "20261101000000.000", ".json")
	install(t, prefix, "gcloud-cli", "575.0.0", "20260706085220.622", ".json")
	symlink(t, filepath.Join(room, "gcloud-cli/594.0.0"), filepath.Join(room, "gcloud-cli/latest"))
	// The latest install stored no definition, so brew does not count the cask.
	install(t, prefix, "half", "1.0", "20200101000000.000", ".json")
	install(t, prefix, "half", "2.0", "20210101000000.000", "")
	mkdir(t, filepath.Join(room, "nometadata/1.0"))
	symlink(t, filepath.Join(room, "slack"), filepath.Join(room, "renamed"))
	if err := os.WriteFile(filepath.Join(room, "stray"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Installed(prefix)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gcloud-cli 594.0.0", "old 1.0", "slack 4.53.292"}
	if !slices.Equal(tokens(got), want) {
		t.Errorf("Installed = %q, want %q", tokens(got), want)
	}
	if got[1].Caskfile != filepath.Join(room, "old/.metadata/1.0/20200101000000.000/Casks/old.rb") {
		t.Errorf("Caskfile = %s", got[1].Caskfile)
	}
}

func TestInstalledWithoutACaskroom(t *testing.T) {
	if got, err := Installed(t.TempDir()); err != nil || got != nil {
		t.Errorf("Installed = %v, %v", got, err)
	}
}

func TestNames(t *testing.T) {
	prefix := t.TempDir()
	room := filepath.Join(prefix, "Caskroom")
	install(t, prefix, "slack", "4.53.292", "20261117083153.956", ".json")
	mkdir(t, filepath.Join(room, "nometadata"))
	mkdir(t, filepath.Join(room, ".hidden"))
	symlink(t, filepath.Join(room, "slack"), filepath.Join(room, "renamed"))
	symlink(t, filepath.Join(room, "gone"), filepath.Join(room, "broken"))

	names, broken, err := Names(prefix)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"broken", "nometadata", "renamed", "slack"}; !slices.Equal(names, want) {
		t.Errorf("names = %q, want %q", names, want)
	}
	if !slices.Equal(broken, []string{"broken"}) {
		t.Errorf("broken = %q", broken)
	}
	if names, broken, err := Names(t.TempDir()); err != nil || names != nil || broken != nil {
		t.Errorf("without a Caskroom: %v, %v, %v", names, broken, err)
	}
}
