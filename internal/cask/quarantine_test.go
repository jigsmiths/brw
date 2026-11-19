package cask

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
)

func quarantineOf(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("/usr/bin/xattr", "-p", quarantineAttr, path).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func TestQuarantineMarksTheDownloadAndWhatItHolds(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("quarantine is macOS only")
	}
	body := tarGzBytes(t, entry{name: "App.app/Contents/Info.plist", body: "x"}, entry{name: "tool", body: "x", mode: 0o755})
	srv, _ := server(t, body, nil)
	f := &Fetcher{Dir: t.TempDir()}
	file := fetch(t, f, api.Cask{Token: "app", URL: srv.URL + "/App.tar.gz", SHA256: sum(body)})
	q := quarantineOf(t, file)
	if !regexp.MustCompile(`^0081;[0-9a-f]{8};;[0-9A-F-]{36}$`).MatchString(q) {
		t.Fatalf("download quarantine = %q", q)
	}
	dest := extract(t, file)
	want := "0181" + strings.TrimPrefix(q, "0081")
	for _, p := range []string{"App.app", "App.app/Contents/Info.plist", "tool"} {
		if got := quarantineOf(t, filepath.Join(dest, p)); got != want {
			t.Errorf("%s: quarantine = %q, want %q", p, got, want)
		}
	}
	if got := quarantineOf(t, dest); got != "" {
		t.Errorf("the destination itself was marked: %q", got)
	}
}

func TestQuarantineKeepsAnExistingValue(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("quarantine is macOS only")
	}
	file := filepath.Join(t.TempDir(), "a.zip")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	const old = "0083;12345678;Safari;"
	if err := exec.Command("/usr/bin/xattr", "-w", quarantineAttr, old, file).Run(); err != nil {
		t.Fatal(err)
	}
	if err := quarantine(context.Background(), file); err != nil {
		t.Fatal(err)
	}
	if got := quarantineOf(t, file); got != old {
		t.Errorf("quarantine = %q, want %q kept", got, old)
	}
}

func TestPropagatedFlags(t *testing.T) {
	for in, want := range map[string]string{
		"0081;6afd42e2;;UUID":    "0181;6afd42e2;;UUID",
		"0083;6afd42e2;Safari;x": "0183;6afd42e2;Safari;x",
		"01c1;6afd42e2;;UUID":    "01c1;6afd42e2;;UUID",
		"zz;bad":                 "",
		"":                       "",
	} {
		if got := propagated(in); got != want {
			t.Errorf("propagated(%q) = %q, want %q", in, got, want)
		}
	}
}
