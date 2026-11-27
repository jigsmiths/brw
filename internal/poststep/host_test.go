package poststep

import (
	"runtime"
	"testing"
)

func TestCurrentHostResolvesEveryFieldOnce(t *testing.T) {
	t.Setenv("HOMEBREW_TEMP", "")
	if h := CurrentHost("darwin", "/Users/me", "27.0.1"); h.Home != "/Users/me" || h.Temp != "/private/tmp" || h.MacVersion != "27.0.1" || h.GOARCH != runtime.GOARCH {
		t.Errorf("darwin host = %+v", h)
	}
	if h := CurrentHost("linux", "/home/me", ""); h.Temp != "/tmp" {
		t.Errorf("linux temp = %q", h.Temp)
	}
	t.Setenv("HOMEBREW_TEMP", "/custom")
	if h := CurrentHost("darwin", "/Users/me", ""); h.Temp != "/custom" {
		t.Errorf("temp = %q, want HOMEBREW_TEMP", h.Temp)
	}
	if h := CurrentHost(runtime.GOOS, "/h", ""); h.Kernel == "" {
		t.Error("the kernel release was not read")
	}
}

func TestAHostWithoutAHomeHasNoHomeBase(t *testing.T) {
	f := newFixture(t)
	f.ctx.Home = ""
	if err := f.run(`[{"type":"mkdir_p","path":{"base":"home","path":"x"}}]`); err == nil {
		t.Error("an unknown home must be an error, not the running user's home")
	}
}
