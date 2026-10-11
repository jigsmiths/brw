// Package info renders what brew info prints for a formula, as brew prints it
// when its output is not a terminal.
package info

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/deps"
	"github.com/jigsmiths/brw/internal/outdated"
	"github.com/jigsmiths/brw/internal/platform"
	"github.com/jigsmiths/brw/internal/version"
)

const coreRemote = "https://github.com/Homebrew/homebrew-core"

// ErrOtherTap is returned for a formula whose installed kegs came from a tap
// that has a formula of the same name in core.
var ErrOtherTap = errors.New("installed from another tap")

// Env is the machine state the output depends on.
type Env struct {
	Index    *api.Index
	Host     platform.Host
	Prefix   cellar.Prefix
	Kegs     []cellar.Keg
	Outdated []outdated.Result
	Now      time.Time // for deprecation dates; the zero value means the current time
}

// keg is an installed keg with its parsed version.
type keg struct {
	cellar.Keg
	pkg    version.PkgVersion
	parsed bool
	scheme int
	head   bool
}

func newKeg(k cellar.Keg) keg {
	out := keg{Keg: k}
	if k.Receipt != nil {
		out.scheme = k.Receipt.VersionScheme()
	}
	pkg, err := version.ParsePkg(k.Version)
	out.pkg, out.parsed = pkg, err == nil
	out.head = out.parsed && pkg.Version.IsHead()
	return out
}

// compare orders kegs by version scheme, then version.
func (a keg) compare(b keg) int {
	switch {
	case a.scheme != b.scheme:
		return a.scheme - b.scheme
	case a.parsed != b.parsed:
		if !a.parsed {
			return -1
		}
		return 1
	case !a.parsed:
		return strings.Compare(a.Version, b.Version)
	}
	return a.pkg.Compare(b.pkg)
}

// Text returns the info text for f.
func Text(f *api.Formula, env Env) (string, error) {
	var b strings.Builder
	tag, err := platform.Tag(env.Host)
	if err != nil {
		return "", err
	}
	here, err := f.ForTag(tag)
	if err != nil {
		return "", err
	}
	kegs := env.installed(f)
	if len(kegs) == 0 && env.hasRack(f.Name) {
		return "", fmt.Errorf("%s: %w", f.Name, ErrOtherTap)
	}
	outdated := env.outdatedResult(f.Name)

	bottled := env.bottledHere(f)
	var specs []string
	if f.Versions.Stable != "" {
		s := "stable " + f.Versions.Stable
		if bottled {
			s += " (bottled)"
		}
		specs = append(specs, s)
	}
	if f.Versions.Head != "" {
		specs = append(specs, "HEAD")
	}
	if outdated != nil && len(specs) > 0 {
		specs[0] = env.installedVersion(f, kegs) + " → " + specs[0]
	}
	fmt.Fprintf(&b, "==> %s: %s", f.FullName, strings.Join(specs, ", "))
	if here.KegOnly {
		b.WriteString(" [keg-only]")
	}
	b.WriteByte('\n')

	if f.Desc != "" {
		b.WriteString(f.Desc + "\n")
	}
	if f.Homepage != "" {
		b.WriteString(f.Homepage + "\n")
	}
	if len(f.Aliases) > 0 {
		b.WriteString("Aliases: " + strings.Join(f.Aliases, ", ") + "\n")
	}
	if len(f.OldNames) > 0 {
		b.WriteString("Old Names: " + strings.Join(f.OldNames, ", ") + "\n")
	}
	if msg := env.deprecation(f); msg != "" {
		b.WriteString(strings.TrimSuffix(msg, "\n") + "\n")
	}
	if c := env.conflicts(f); len(c) > 0 {
		b.WriteString("Conflicts with:\n  " + strings.Join(c, "\n  ") + "\n")
	}

	if len(kegs) == 0 {
		b.WriteString("Not installed\n")
	} else {
		b.WriteString(env.installStatus(f, kegs) + "\n")
	}
	fmt.Fprintf(&b, "From: %s/blob/HEAD/%s\n", coreRemote, f.RubySourcePath)
	if f.License != "" {
		b.WriteString("License: " + f.License + "\n")
	}

	rows, err := env.installedVersions(f)
	if err != nil {
		return "", err
	}
	if len(rows) > 0 {
		b.WriteString("==> Installed Versions\n" + strings.Join(rows, "\n") + "\n")
	}

	dl, err := env.dependencies(f, kegs, bottled)
	if err != nil {
		return "", err
	}
	b.WriteString(dl)

	if f.Versions.Head != "" {
		b.WriteString("==> Options\n--HEAD\n\tInstall HEAD version\n")
	}
	if f.Caveats != "" {
		text := strings.NewReplacer("$HOMEBREW_PREFIX", string(env.Prefix), "$HOMEBREW_CELLAR", string(env.Prefix)+"/Cellar").Replace(f.Caveats)
		b.WriteString("==> Caveats\n" + strings.TrimSuffix(text, "\n") + "\n")
	}
	return b.String(), nil
}

var deprecationReasons = map[string]string{
	"does_not_build":      "does not build",
	"no_license":          "has no license",
	"repo_archived":       "has an archived upstream repository",
	"repo_removed":        "has a removed upstream repository",
	"unmaintained":        "is not maintained upstream",
	"unreachable":         "is no longer reliably reachable upstream",
	"unsupported":         "is not supported upstream",
	"deprecated_upstream": "is deprecated upstream",
	"versioned_formula":   "is a versioned formula",
	"checksum_mismatch": "was built with an initially released source file that had a different checksum than the current one. " +
		"Upstream's repository might have been compromised. " +
		"We can re-package this once upstream has confirmed that they retagged their release",
}

// deprecation is the notice for a deprecated or disabled formula, or "".
func (e Env) deprecation(f *api.Formula) string {
	var kind, reason, replacement string
	switch {
	case f.Deprecated:
		kind, reason = "Deprecated", f.DeprecationReason
		replacement = replacementWithType(f.DeprecationReplacementFormula, f.DeprecationReplacementCask)
	case f.Disabled:
		kind, reason = "Disabled", f.DisableReason
		replacement = replacementWithType(f.DisableReplacementFormula, f.DisableReplacementCask)
	default:
		return ""
	}
	if r, ok := deprecationReasons[reason]; ok {
		reason = r
	}
	msg := kind + "!"
	if reason != "" {
		msg = kind + " because it " + reason + "!"
	}

	disableDate := f.DisableDate
	if disableDate == "" && f.DeprecationDate != "" {
		if d, err := time.Parse(time.DateOnly, f.DeprecationDate); err == nil {
			disableDate = plusYear(d).Format(time.DateOnly)
		}
	}
	if disableDate != "" {
		now := e.Now
		if now.IsZero() {
			now = time.Now()
		}
		if disableDate < now.Format(time.DateOnly) {
			msg += " It was disabled on " + disableDate + "."
		} else {
			msg += " It will be disabled on " + disableDate + "."
		}
	}
	if replacement != "" {
		msg += "\nReplacement:\n  brew install " + replacement + "\n"
	}
	return msg
}

// plusYear adds twelve months, clamping to the end of a shorter month.
func plusYear(d time.Time) time.Time {
	next := time.Date(d.Year()+1, d.Month(), 1, 0, 0, 0, 0, time.UTC)
	last := next.AddDate(0, 1, -1).Day()
	return time.Date(next.Year(), next.Month(), min(d.Day(), last), 0, 0, 0, 0, time.UTC)
}

func replacementWithType(formula, cask string) string {
	switch {
	case formula != "" && formula == cask:
		return formula
	case formula != "":
		return "--formula " + formula
	case cask != "":
		return "--cask " + cask
	}
	return ""
}

// conflicts lists the formulae f conflicts with and why, sorted.
func (e Env) conflicts(f *api.Formula) []string {
	var out []string
	for i, name := range f.ConflictsWith {
		if other, ok := e.Index.Lookup(name); ok {
			if other.FullName == f.FullName {
				continue
			}
			name = other.FullName
		}
		if i < len(f.ConflictsWithReasons) && f.ConflictsWithReasons[i] != nil {
			name += " (because " + *f.ConflictsWithReasons[i] + ")"
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// installed returns f's kegs, oldest first, with HEAD kegs ahead of the rest.
func (e Env) installed(f *api.Formula) []keg {
	var kegs []keg
	var named []cellar.Keg
	for _, k := range e.Kegs {
		if k.Name == f.Name {
			named = append(named, k)
		}
	}
	for _, k := range outdated.FromTap(named, f.Tap) {
		kegs = append(kegs, newKeg(k))
	}
	var heads, versioned []keg
	for _, k := range kegs {
		if k.head {
			heads = append(heads, k)
		} else {
			versioned = append(versioned, k)
		}
	}
	slices.SortStableFunc(heads, func(a, b keg) int { return int(receiptTime(b) - receiptTime(a)) })
	slices.SortStableFunc(versioned, keg.compare)
	return append(heads, versioned...)
}

func receiptTime(k keg) int64 {
	if k.Receipt == nil {
		return 0
	}
	return k.Receipt.Time
}

func (e Env) outdatedResult(name string) *outdated.Result {
	for i := range e.Outdated {
		if e.Outdated[i].Name == name {
			return &e.Outdated[i]
		}
	}
	return nil
}

// bottledHere reports whether f has a bottle this host can use.
func (e Env) bottledHere(f *api.Formula) bool {
	_, ok := platform.Select(e.Host, f.BottleTags())
	return ok
}

// installedVersion is the linked version, or else the newest installed one.
func (e Env) installedVersion(f *api.Formula, kegs []keg) string {
	for _, k := range kegs {
		if e.Prefix.Linked(f.Name, k.Version) {
			return k.Version
		}
	}
	return kegs[len(kegs)-1].Version
}

// installStatus reports how the keg brew reads the receipt from was installed.
func (e Env) installStatus(f *api.Formula, kegs []keg) string {
	tab := kegs[len(kegs)-1]
	if v, ok := e.Prefix.OptVersion(f.Name); ok {
		for _, k := range kegs {
			if k.Version == v {
				tab = k
			}
		}
	}
	if tab.Receipt != nil && tab.Receipt.InstalledOnRequest {
		return "Installed (on request)"
	}
	return "Installed (as dependency)"
}

var versionedName = regexp.MustCompile(`@[\d.]+$`)

// installedVersions lists the installed kegs of f and its related versioned
// formulae, newest formula first.
func (e Env) installedVersions(f *api.Formula) ([]string, error) {
	related := []*api.Formula{f}
	if versionedName.MatchString(f.Name) {
		if p, ok := e.Index.Lookup(versionedName.ReplaceAllString(f.Name, "")); ok {
			related = append(related, p)
		}
	}
	for _, n := range f.VersionedFormulae {
		if s, ok := e.Index.Lookup(n); ok {
			related = append(related, s)
		}
	}

	type group struct {
		f    *api.Formula
		kegs []keg // newest first
	}
	var groups []group
	seen := map[string]bool{}
	for _, r := range related {
		if seen[r.FullName] {
			continue
		}
		seen[r.FullName] = true
		kegs := e.installed(r)
		if len(kegs) == 0 {
			continue
		}
		// installed puts HEAD first then oldest to newest; show newest first.
		var heads, rest []keg
		for _, k := range kegs {
			if k.head {
				heads = append(heads, k)
			} else {
				rest = append(rest, k)
			}
		}
		slices.Reverse(rest)
		groups = append(groups, group{r, append(heads, rest...)})
	}
	if len(groups) == 0 {
		return nil, nil
	}
	newest := func(g group) keg {
		best := g.kegs[0]
		for _, k := range g.kegs[1:] {
			if k.compare(best) > 0 {
				best = k
			}
		}
		return best
	}
	slices.SortStableFunc(groups, func(a, b group) int { return newest(b).compare(newest(a)) })

	var rows []row
	for _, g := range groups {
		res := e.outdatedResult(g.f.Name)
		latest, err := version.NewPkg(g.f.Versions.Stable, g.f.Revision)
		if err != nil {
			return nil, err
		}
		for i, k := range g.kegs {
			linked := e.Prefix.Linked(g.f.Name, k.Version)
			if i != 0 && !linked {
				continue
			}
			abv, err := k.Abv()
			if err != nil {
				return nil, err
			}
			v := k.Version
			if i == 0 && res != nil && latest.String() != v {
				v += " → " + latest.String()
			}
			r := row{name: g.f.FullName, version: v, size: "(" + abv + ")"}
			if linked {
				r.linked = "[Linked]"
			}
			rows = append(rows, r)
		}
	}
	return formatRows(rows), nil
}

// row is one line of the Installed Versions block.
type row struct{ name, version, size, linked string }

// formatRows aligns the columns. Only rows with a [Linked] marker pad the size.
func formatRows(rows []row) []string {
	var nameW, versionW, sizeW int
	for _, r := range rows {
		nameW = max(nameW, utf8.RuneCountInString(r.name))
		versionW = max(versionW, utf8.RuneCountInString(r.version))
		sizeW = max(sizeW, utf8.RuneCountInString(r.size))
	}
	lines := make([]string, len(rows))
	for i, r := range rows {
		size := r.size
		if r.linked != "" {
			size = fmt.Sprintf("%-*s", sizeW, size)
		}
		line := fmt.Sprintf("%-*s %-*s %s", nameW, r.name, versionW, r.version, size)
		if r.linked != "" {
			line += " " + r.linked
		}
		lines[i] = line
	}
	return lines
}

// dependencies renders the Dependencies block, or nothing if it is empty.
func (e Env) dependencies(f *api.Formula, kegs []keg, bottled bool) (string, error) {
	r, err := deps.NewResolver(e.Index, e.Host)
	if err != nil {
		return "", err
	}
	typed, err := r.Typed(f.Name)
	if err != nil {
		return "", err
	}

	hideBuild := false
	if len(kegs) > 0 {
		hideBuild = true
		for _, k := range kegs {
			if k.Receipt == nil || !k.Receipt.PouredFromBottle {
				hideBuild = false
			}
		}
	} else {
		hideBuild = bottled || slices.ContainsFunc(f.Requirements, func(r struct{ Name string }) bool {
			return r.Name == "macos" || r.Name == "linux"
		})
	}

	var lines []string
	add := func(kind string, names []string) {
		if len(names) > 0 {
			lines = append(lines, fmt.Sprintf("%s (%d): %s", kind, len(names), strings.Join(names, ", ")))
		}
	}
	if !hideBuild {
		add("Build", typed.Build)
	}
	add("Required", typed.Required)
	add("Recommended", typed.Recommended)
	add("Optional", typed.Optional)

	var runtime []cellar.Dependency
	if len(kegs) > 0 && kegs[len(kegs)-1].Receipt != nil {
		runtime = kegs[len(kegs)-1].Receipt.RuntimeDependencies
	}
	if len(runtime) > 0 {
		installed := 0
		for _, d := range runtime {
			if e.hasRack(d.FullName) {
				installed++
			}
		}
		lines = append(lines, fmt.Sprintf("Recursive Runtime (%d): %s", len(runtime), dependencyStatus(installed, len(runtime))))
	}
	if len(lines) == 0 {
		return "", nil
	}
	return "==> Dependencies\n" + strings.Join(lines, "\n") + "\n", nil
}

func (e Env) hasRack(fullName string) bool {
	name := fullName[strings.LastIndex(fullName, "/")+1:]
	for _, k := range e.Kegs {
		if k.Name == name {
			return true
		}
	}
	return false
}

func dependencyStatus(installed, total int) string {
	if installed == total {
		return "all installed ✔"
	}
	return fmt.Sprintf("%d installed ✔, %d missing ✘", installed, total-installed)
}
