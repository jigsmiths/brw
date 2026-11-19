package outdated

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/caskroom"
	"github.com/jigsmiths/brw/internal/version"
)

// CaskOptions are brew outdated's switches for casks.
type CaskOptions struct {
	Greedy            bool     // --greedy
	GreedyLatest      bool     // --greedy-latest
	GreedyAutoUpdates bool     // --greedy-auto-updates
	GreedyTokens      []string // HOMEBREW_UPGRADE_GREEDY_CASKS: greedy for these casks only
	NoAutoUpdates     bool     // HOMEBREW_NO_UPGRADE_AUTO_UPDATES_CASKS

	Tag string // the platform tag, for the cask's variations
	// AppDir is where an installed cask's apps went (caskroom.AppDir).
	AppDir func(token string) string
	// Bundle reads the CFBundleShortVersionString and CFBundleVersion of an app,
	// "" for each it lacks. ok is false when the app has no readable Info.plist.
	Bundle func(app string) (short, long string, ok bool)
	Home   string // for app targets under ~
}

// CheckCasks returns the outdated casks in the order given. Casks the API
// does not know, or that were installed from another tap, are skipped. A
// version "latest" cask is never reported: brew downloads it to tell, and brw
// does not.
func CheckCasks(idx *api.CaskIndex, prefix string, installed []caskroom.Cask, o CaskOptions) ([]Result, error) {
	var out []Result
	for _, ic := range installed {
		c, ok := idx.Lookup(ic.Token)
		if !ok || (ic.Tap != "" && ic.Tap != c.Tap) {
			continue
		}
		cask, err := c.ForTag(o.Tag)
		if err != nil {
			return nil, err
		}
		if !caskOutdated(cask, ic, o) {
			continue
		}
		r := Result{Name: cask.Token, InstalledVersions: []string{ic.Version}, CurrentVersion: cask.Version}
		r.PinnedVersion, r.Pinned = caskroom.Pinned(prefix, ic.Token)
		out = append(out, r)
	}
	return out, nil
}

// caskOutdated follows brew's Cask#outdated_version.
func caskOutdated(c api.Cask, ic caskroom.Cask, o CaskOptions) bool {
	if c.Disabled || c.Version == "latest" || ic.Version == c.Version {
		return false
	}
	greedy := o.Greedy || slices.Contains(o.GreedyTokens, c.Token)
	if c.AutoUpdates && !greedy && !o.GreedyAutoUpdates {
		return !o.NoAutoUpdates && bundleOutdated(c, ic, o)
	}
	return true
}

// badBundleVersions are placeholders some apps ship instead of a version.
var badBundleVersions = []string{"0", "0.0"}

// bundleOutdated follows brew's Cask#auto_updates_bundle_outdated?: whether the
// app a self-updating cask installed is behind the listing even so.
func bundleOutdated(c api.Cask, ic caskroom.Cask, o CaskOptions) bool {
	app, ok := appTarget(c, o.AppDir(ic.Token), o.Home)
	if !ok {
		return false
	}
	short, long, ok := o.Bundle(app)
	if !ok {
		return false
	}
	if slices.Contains(badBundleVersions, short) {
		short = ""
	}
	if slices.Contains(badBundleVersions, long) {
		long = ""
	}
	if short == "" && long == "" {
		return false
	}
	csv := strings.Split(c.Version, ",")
	tapShort := csv[0]
	if tapShort == "" {
		tapShort = c.Version
	}

	// Some apps split a cask version like 2.61-2057 across the two fields.
	if short != "" && long != "" {
		combined := compareEach(short+"-"+long, csv)
		if slices.Contains(combined, 0) {
			return false
		}
		if len(combined) > 0 && !slices.Contains(combined, -1) {
			return false
		}
		if len(combined) == 0 && short == beforeLast(tapShort, "-") {
			return false
		}
	}
	for _, v := range []string{short, long} {
		if n, ok := compareStrings(v, tapShort); ok && n == 0 {
			return false
		}
	}
	if n, ok := compareStrings(short, tapShort); ok {
		return n < 0
	}
	// A short version still matching Homebrew's install record means a
	// different tap version is outdated, as for casks without auto_updates.
	if len(csv) == 1 && short != "" && short == ic.Version {
		return true
	}
	builds := compareEach(long, csv)
	return len(builds) > 0 && !slices.Contains(builds, 0) && slices.Contains(builds, -1)
}

// compareEach compares v with each candidate, leaving out those that cannot be.
func compareEach(v string, candidates []string) []int {
	var out []int
	for _, c := range candidates {
		if n, ok := compareStrings(v, c); ok {
			out = append(out, n)
		}
	}
	return out
}

// compareStrings orders two versions as brew's compare_version_strings does:
// not at all when either is blank or they have a different number of parts.
func compareStrings(a, b string) (int, bool) {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" || dotParts(a) != dotParts(b) {
		return 0, false
	}
	va, err := version.Parse(a)
	if err != nil {
		return 0, false
	}
	vb, err := version.Parse(b)
	if err != nil {
		return 0, false
	}
	return va.Compare(vb), true
}

// dotParts counts the parts of s split on dots as Ruby's String#split does,
// which drops empty trailing parts.
func dotParts(s string) int {
	parts := strings.Split(s, ".")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return len(parts)
}

// beforeLast is Ruby's s.rpartition(sep).first.
func beforeLast(s, sep string) string {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i]
	}
	return ""
}

// appTarget is where the cask's only app was put. A cask with no app, or with
// several, has none brew would check.
func appTarget(c api.Cask, appDir, home string) (string, bool) {
	var apps [][]json.RawMessage
	for _, raw := range c.Artifacts {
		var a struct {
			App []json.RawMessage `json:"app"`
		}
		if json.Unmarshal(raw, &a) == nil && a.App != nil {
			apps = append(apps, a.App)
		}
	}
	if len(apps) != 1 || len(apps[0]) == 0 {
		return "", false
	}
	var source string
	if json.Unmarshal(apps[0][0], &source) != nil || source == "" {
		return "", false
	}
	target := filepath.Base(source)
	if len(apps[0]) > 1 {
		var opts struct {
			Target string `json:"target"`
		}
		if json.Unmarshal(apps[0][1], &opts) == nil && opts.Target != "" {
			target = opts.Target
		}
	}
	switch {
	case filepath.IsAbs(target):
		return target, true
	case target == "~" || strings.HasPrefix(target, "~/"):
		return filepath.Join(home, strings.TrimPrefix(target, "~")), true
	}
	return filepath.Join(appDir, target), true
}

// ReadBundle reads an app's versions with plutil, which reads every plist
// format, as brew does.
func ReadBundle(app string) (short, long string, ok bool) {
	plist := filepath.Join(app, "Contents", "Info.plist")
	f, err := os.Open(plist)
	if err != nil {
		return "", "", false
	}
	_ = f.Close()
	return plistString(plist, "CFBundleShortVersionString"), plistString(plist, "CFBundleVersion"), true
}

// plistString is "" when the key is missing or plutil fails, which brew also
// treats as no version.
func plistString(plist, key string) string {
	out, err := exec.Command("/usr/bin/plutil", "-extract", key, "raw", "-o", "-", plist).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
