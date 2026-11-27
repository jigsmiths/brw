package caskroom

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKeyboardLayoutDirPrefersTheExplicitOptionThenTheDefault(t *testing.T) {
	prefix := t.TempDir()
	meta := filepath.Join(dir(prefix), "layout", ".metadata")
	if err := os.MkdirAll(meta, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := KeyboardLayoutDir(prefix, "layout"); got != "/Library/Keyboard Layouts" {
		t.Errorf("without a config = %q", got)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(meta, "config.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"default":{"keyboard_layoutdir":"/Library/Keyboard Layouts"},"explicit":{}}`)
	if got := KeyboardLayoutDir(prefix, "layout"); got != "/Library/Keyboard Layouts" {
		t.Errorf("default = %q", got)
	}
	write(`{"default":{"keyboard_layoutdir":"/Library/Keyboard Layouts"},"explicit":{"keyboard_layoutdir":"/tmp/layouts"}}`)
	if got := KeyboardLayoutDir(prefix, "layout"); got != "/tmp/layouts" {
		t.Errorf("explicit = %q", got)
	}
}
