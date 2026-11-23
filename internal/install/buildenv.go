package install

import (
	"bytes"
	"compress/gzip"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type stdenvParams struct {
	Prefix, Buildpath, Cache, SDK, MacVersion, Temp string
	Jobs                                            int
	DepBins                                         []string // opt bin directories
	Keg                                             string
	SourceModified                                  time.Time
}

// packageCaches are Homebrew::PackageManagerCache's variables and folders.
var packageCaches = map[string]string{
	"BUNDLE_USER_CACHE": "bundler_cache", "CABAL_DIR": "cabal_cache", "CARGO_HOME": "cargo_cache",
	"COMPOSER_CACHE_DIR": "composer_cache", "GEM_SPEC_CACHE": "gem_cache", "GOCACHE": "go_cache",
	"GOPATH": "go_mod_cache", "HEX_HOME": "hex_cache", "NPM_CONFIG_CACHE": "npm_cache",
	"NPM_CONFIG_STORE_DIR": "pnpm_cache", "NUGET_PACKAGES": "nuget_cache", "PIP_CACHE_DIR": "pip_cache",
	"UV_CACHE_DIR": "uv_cache", "YARN_CACHE_FOLDER": "yarn_cache", "ZIG_GLOBAL_CACHE_DIR": "zig_cache",
}

// stdenv is brew's Stdenv, not the default superenv, whose compiler shims are Ruby.
func stdenv(p stdenvParams) map[string]string {
	flags := "-Os -w -pipe -mmacosx-version-min=" + p.MacVersion + " -isysroot" + p.SDK
	home := filepath.Join(p.Buildpath, ".brew_home")
	fwFlag, fwPath := "", ""
	if fi, err := os.Stat(filepath.Join(p.Prefix, "Frameworks")); err == nil && fi.IsDir() {
		fwFlag, fwPath = " -F"+filepath.Join(p.Prefix, "Frameworks"), filepath.Join(p.Prefix, "Frameworks")+":"
	}
	env := map[string]string{
		"HOMEBREW_ENV": "std",
		"PATH":         strings.Join(append(append([]string{}, p.DepBins...), "/usr/bin", "/bin", "/usr/sbin", "/sbin"), ":"),
		"MAKEFLAGS":    "-j" + strconv.Itoa(p.Jobs),
		"CC":           "/usr/bin/clang", "CXX": "/usr/bin/clang++", "OBJC": "/usr/bin/clang", "OBJCXX": "/usr/bin/clang++",
		"CFLAGS": flags, "CXXFLAGS": flags, "OBJCFLAGS": flags, "OBJCXXFLAGS": flags,
		"CPPFLAGS":             "-isystem" + p.Prefix + "/include" + fwFlag + " -isysroot" + p.SDK,
		"LDFLAGS":              "-L" + p.Prefix + "/lib" + fwFlag + " -Wl,-headerpad_max_install_names -isysroot" + p.SDK,
		"CPATH":                p.Prefix + "/include:" + p.SDK + "/usr/include",
		"SDKROOT":              p.SDK,
		"LC_CTYPE":             "C",
		"CMAKE_PREFIX_PATH":    p.Prefix + ":" + p.SDK + "/usr",
		"CMAKE_FRAMEWORK_PATH": fwPath + p.SDK + "/System/Library/Frameworks",
	}
	maps.Copy(env, sandboxEnv(home, p.Cache, p.Temp))
	env["HOMEBREW_FORMULA_PREFIX"] = p.Keg
	env["HOMEBREW_FORMULA_BUILDPATH"] = p.Buildpath
	env["SOURCE_DATE_EPOCH"] = strconv.FormatInt(p.SourceModified.Unix(), 10)
	env["TZ"] = "UTC0"
	var pc []string
	for _, d := range []string{p.Prefix + "/lib/pkgconfig", p.Prefix + "/share/pkgconfig", "/usr/lib/pkgconfig"} {
		if _, err := os.Stat(d); err == nil {
			pc = append(pc, d)
		}
	}
	env["PKG_CONFIG_LIBDIR"] = strings.Join(pc, ":")
	return env
}

// tarCommit is Utils::Git.get_tar_commit_id: the commit git archive writes
// into the first 1024 bytes of a tarball, as GitHub's archives have, or "".
func tarCommit(file string) string {
	name := filepath.Base(file)
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	var r io.Reader
	switch {
	case strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz"):
		zr, err := gzip.NewReader(f)
		if err != nil {
			return ""
		}
		r = zr
	case strings.HasSuffix(name, ".tar"):
		r = f
	default:
		return ""
	}
	header := make([]byte, 1024)
	if _, err := io.ReadFull(r, header); err != nil {
		return ""
	}
	cmd := exec.Command("git", "get-tar-commit-id")
	cmd.Stdin = bytes.NewReader(header)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// sandboxEnv is brew's common_sandbox_env.
func sandboxEnv(home, cache, temp string) map[string]string {
	env := map[string]string{
		"HOME": home, "TMPDIR": temp, "TEMP": temp, "TMP": temp,
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_TERMINAL_PROMPT": "0", "GOENV": "off",
		"CARGO_REGISTRY_GLOBAL_MIN_PUBLISH_AGE": "1 day", "BUNDLE_COOLDOWN": "1", "BUNDLE_GLOBAL_GEM_CACHE": "true",
		"PIP_CONFIG_FILE": os.DevNull, "NPM_CONFIG_USERCONFIG": os.DevNull, "CURL_HOME": home,
		"PYTHONDONTWRITEBYTECODE": "1", "XDG_CONFIG_HOME": home + "/.config",
		"_JAVA_OPTIONS": "-Duser.home=" + filepath.Join(cache, "java_cache") + " -Djava.io.tmpdir=" + temp,
	}
	for k, dir := range packageCaches {
		env[k] = filepath.Join(cache, dir)
	}
	return env
}
