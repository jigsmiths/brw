// Package platform maps a host to Homebrew bottle tags.
package platform

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

type Host struct {
	OS         string // GOOS: "darwin" or "linux"
	MacVersion string // product version, e.g. "27.0.1"; darwin only
	Arch       string // GOARCH: "arm64" or "amd64"
}

// newest first
var macReleases = []struct {
	major int
	name  string
}{
	{27, "golden_gate"},
	{26, "tahoe"},
	{15, "sequoia"},
	{14, "sonoma"},
	{13, "ventura"},
	{12, "monterey"},
	{11, "big_sur"},
}

// Current returns the Host for this process; macVersion is ignored off macOS.
func Current(macVersion string) Host {
	return Host{OS: runtime.GOOS, MacVersion: macVersion, Arch: runtime.GOARCH}
}

func Tag(h Host) (string, error) {
	switch h.OS {
	case "linux":
		return linuxTag(h.Arch)
	case "darwin":
		major, err := macMajor(h.MacVersion)
		if err != nil {
			return "", err
		}
		for _, r := range macReleases {
			if r.major == major {
				return macTag(h.Arch, r.name)
			}
		}
		return "", fmt.Errorf("unsupported macOS version %q", h.MacVersion)
	}
	return "", fmt.Errorf("unsupported OS %q", h.OS)
}

// Select picks the best of available for h: the exact tag, then the newest
// older macOS tag of the same arch, then "all".
func Select(h Host, available []string) (string, bool) {
	have := make(map[string]bool, len(available))
	for _, a := range available {
		have[a] = true
	}
	for _, tag := range candidates(h) {
		if have[tag] {
			return tag, true
		}
	}
	if have["all"] {
		return "all", true
	}
	return "", false
}

func candidates(h Host) []string {
	switch h.OS {
	case "linux":
		tag, err := linuxTag(h.Arch)
		if err != nil {
			return nil
		}
		return []string{tag}
	case "darwin":
		major, err := macMajor(h.MacVersion)
		if err != nil {
			return nil
		}
		if _, err := Tag(h); err != nil {
			// Unknown macOS versions match no tag.
			return nil
		}
		var out []string
		for _, r := range macReleases {
			if r.major > major {
				continue
			}
			if tag, err := macTag(h.Arch, r.name); err == nil {
				out = append(out, tag)
			}
		}
		return out
	}
	return nil
}

func linuxTag(arch string) (string, error) {
	switch arch {
	case "amd64":
		return "x86_64_linux", nil
	case "arm64":
		return "arm64_linux", nil
	}
	return "", fmt.Errorf("unsupported Linux arch %q", arch)
}

func macTag(arch, name string) (string, error) {
	switch arch {
	case "arm64":
		return "arm64_" + name, nil
	case "amd64":
		return name, nil
	}
	return "", fmt.Errorf("unsupported macOS arch %q", arch)
}

// macMajor parses the major version; 10.x is unsupported.
func macMajor(version string) (int, error) {
	first, _, _ := strings.Cut(version, ".")
	major, err := strconv.Atoi(first)
	if err != nil {
		return 0, fmt.Errorf("invalid macOS version %q", version)
	}
	if major < 11 {
		return 0, fmt.Errorf("unsupported macOS version %q", version)
	}
	return major, nil
}
