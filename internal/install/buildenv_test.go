package install

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStdenvIsBrewsStdenvAndSandbox(t *testing.T) {
	p := t.TempDir()
	params := stdenvParams{Prefix: p, Buildpath: "/b/tool-1.0", Cache: "/c", SDK: "/sdk", MacVersion: "27.2",
		Jobs: 8, DepBins: []string{p + "/opt/go/bin"}, Temp: "/private/tmp"}
	got := stdenv(params)
	sdkFlags := "-Os -w -pipe -mmacosx-version-min=27.2 -isysroot/sdk"
	for k, want := range map[string]string{
		"HOMEBREW_ENV": "std", "PATH": p + "/opt/go/bin:/usr/bin:/bin:/usr/sbin:/sbin", "MAKEFLAGS": "-j8",
		"CC": "/usr/bin/clang", "CXX": "/usr/bin/clang++", "OBJC": "/usr/bin/clang", "OBJCXX": "/usr/bin/clang++",
		"CFLAGS": sdkFlags, "CXXFLAGS": sdkFlags, "OBJCFLAGS": sdkFlags, "OBJCXXFLAGS": sdkFlags,
		"CPPFLAGS": "-isystem" + p + "/include -isysroot/sdk",
		"LDFLAGS":  "-L" + p + "/lib -Wl,-headerpad_max_install_names -isysroot/sdk",
		"CPATH":    p + "/include:/sdk/usr/include", "SDKROOT": "/sdk", "LC_CTYPE": "C",
		"CMAKE_PREFIX_PATH": p + ":/sdk/usr", "CMAKE_FRAMEWORK_PATH": "/sdk/System/Library/Frameworks",
		"HOME": "/b/tool-1.0/.brew_home", "TMPDIR": "/private/tmp", "GOENV": "off", "GIT_TERMINAL_PROMPT": "0",
		"GIT_CONFIG_GLOBAL": "/dev/null", "XDG_CONFIG_HOME": "/b/tool-1.0/.brew_home/.config",
		"GOCACHE": "/c/go_cache", "GOPATH": "/c/go_mod_cache", "CARGO_HOME": "/c/cargo_cache",
		"CARGO_REGISTRY_GLOBAL_MIN_PUBLISH_AGE": "1 day", "PYTHONDONTWRITEBYTECODE": "1",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}

	// A prefix with Frameworks adds them before the SDK, as Stdenv does.
	if err := os.Mkdir(filepath.Join(p, "Frameworks"), 0o755); err != nil {
		t.Fatal(err)
	}
	got = stdenv(params)
	for k, want := range map[string]string{
		"CPPFLAGS":             "-isystem" + p + "/include -F" + p + "/Frameworks -isysroot/sdk",
		"LDFLAGS":              "-L" + p + "/lib -F" + p + "/Frameworks -Wl,-headerpad_max_install_names -isysroot/sdk",
		"CMAKE_FRAMEWORK_PATH": p + "/Frameworks:/sdk/System/Library/Frameworks",
	} {
		if got[k] != want {
			t.Errorf("with Frameworks, %s = %q, want %q", k, got[k], want)
		}
	}
}
