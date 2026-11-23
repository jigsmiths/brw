package info

import (
	"slices"
	"strconv"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/platform"
)

// stableRequirements keeps the kinds brew can read from the API.
func stableRequirements(f *api.Formula) []api.Requirement {
	var out []api.Requirement
	for _, r := range f.Requirements {
		switch r.Name {
		case "arch", "linux", "macos", "maximum_macos", "xcode":
		default:
			continue
		}
		if slices.Contains(r.Specs, "stable") {
			out = append(out, r)
		}
	}
	return out
}

// requirementText is brew's display_s for a requirement.
func requirementText(r api.Requirement) string {
	switch r.Name {
	case "arch":
		return r.Version + " architecture"
	case "linux":
		return "Linux"
	case "macos", "maximum_macos":
		if r.Version == "" {
			return "macOS"
		}
		cmp := ">="
		if r.Name == "maximum_macos" {
			cmp = "<="
		}
		return "macOS " + cmp + " " + r.Version
	default:
		if r.Version == "" {
			return "Xcode (on macOS)"
		}
		return "Xcode >= " + r.Version + " (on macOS)"
	}
}

func requirements(f *api.Formula) string {
	var build, required []string
	for _, r := range stableRequirements(f) {
		if slices.Contains(r.Contexts, "build") {
			build = append(build, requirementText(r))
		} else {
			required = append(required, requirementText(r))
		}
	}
	var b strings.Builder
	if len(build) > 0 {
		b.WriteString("Build: " + strings.Join(build, ", ") + "\n")
	}
	if len(required) > 0 {
		b.WriteString("Required: " + strings.Join(required, ", ") + "\n")
	}
	if b.Len() == 0 {
		return ""
	}
	return "==> Requirements\n" + b.String()
}

func incompatible(f *api.Formula, h platform.Host) bool {
	mac := h.OS == "darwin"
	major, _ := platform.MacMajor(h.MacVersion)
	for _, r := range stableRequirements(f) {
		if slices.Contains(r.Contexts, "build") {
			continue
		}
		switch r.Name {
		case "arch":
			if r.Version != hostArch(h) {
				return true
			}
		case "linux":
			if mac {
				return true
			}
		case "macos", "maximum_macos":
			if !mac {
				return true
			}
			if v, err := strconv.ParseFloat(r.Version, 64); err == nil {
				if r.Name == "macos" && float64(major) < v || r.Name == "maximum_macos" && float64(major) > v {
					return true
				}
			}
		}
	}
	return false
}

func hostArch(h platform.Host) string {
	if h.Arch == "amd64" {
		return "x86_64"
	}
	return h.Arch
}
