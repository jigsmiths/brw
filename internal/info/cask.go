package info

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cask"
	"github.com/jigsmiths/brw/internal/caskroom"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/outdated"
	"github.com/jigsmiths/brw/internal/platform"
)

const caskRemote = "https://github.com/Homebrew/homebrew-cask"

// CaskEnv is the machine state a cask's info depends on.
type CaskEnv struct {
	Casks     *api.CaskIndex
	Host      platform.Host
	Prefix    string
	Installed []caskroom.Cask
	Kegs      []cellar.Keg
	// Outdated is applied as brew outdated without --greedy; CaskText sets its Tag.
	Outdated outdated.CaskOptions
	Home     string
	Rosetta  bool      // whether Rosetta 2 is installed
	Now      time.Time // for deprecation dates; the zero value means the current time
}

// CaskText returns brew info's non-terminal output for a cask, without analytics.
func CaskText(c *api.Cask, env CaskEnv) (string, error) {
	tag, err := platform.Tag(env.Host)
	if err != nil {
		return "", err
	}
	env.Outdated.Tag = tag
	cask, err := c.ForTag(tag)
	if err != nil {
		return "", err
	}
	ic, installed := env.installed(cask)
	if installed && ic.Tap != "" && ic.Tap != cask.Tap {
		return "", fmt.Errorf("%s: %w", cask.Token, ErrOtherTap)
	}
	appDir := "/Applications"
	if installed {
		appDir = caskroom.AppDir(env.Prefix, ic.Token)
	}

	var b strings.Builder
	b.WriteString("==> " + cask.Token)
	if len(cask.Names) > 0 {
		b.WriteString(" (" + strings.Join(cask.Names, ", ") + ")")
	}
	b.WriteString(": ")
	if installed && env.isOutdated(ic) {
		b.WriteString(ic.Version + " → ")
	}
	b.WriteString(cask.Version)
	if cask.AutoUpdates {
		b.WriteString(" (auto_updates)")
	}
	b.WriteByte('\n')

	if cask.Desc != "" {
		b.WriteString(cask.Desc + "\n")
	}
	if cask.Homepage != "" {
		b.WriteString(cask.Homepage + "\n")
	}
	now := env.Now
	if now.IsZero() {
		now = time.Now()
	}
	if msg := CaskDeprecation(&cask, now); msg != "" {
		b.WriteString(strings.TrimSuffix(msg, "\n") + "\n")
	}
	switch {
	case !installed:
		b.WriteString("Not installed\n")
	case ic.OnRequest:
		b.WriteString("Installed (on request)\n")
	default:
		b.WriteString("Installed (as dependency)\n")
	}
	if cask.Tap == "homebrew/cask" && cask.RubySourcePath != "" {
		fmt.Fprintf(&b, "From: %s/blob/HEAD/%s\n", caskRemote, cask.RubySourcePath)
	}
	if rows := env.installedRows(cask); len(rows) > 0 {
		b.WriteString("==> Installed Versions\n" + strings.Join(rows, "\n") + "\n")
	}
	b.WriteString(env.caskDependencies(cask, tag))
	b.WriteString(caskRequirements(cask))
	if len(cask.Languages) > 0 {
		b.WriteString("==> Languages\n" + strings.Join(cask.Languages, ", ") + "\n")
	}
	if a := artifactSections(cask, env.placeholders(appDir)); a != "" {
		b.WriteString(a + "\n")
	}
	if c := env.caskCaveats(cask, appDir); c != "" {
		b.WriteString("==> Caveats\n" + c + "\n")
	}
	return b.String(), nil
}

// installed finds the install of c, under its token or an old one.
func (e CaskEnv) installed(c api.Cask) (caskroom.Cask, bool) {
	for _, token := range append([]string{c.Token}, c.OldTokens...) {
		for _, ic := range e.Installed {
			if ic.Token == token {
				return ic, true
			}
		}
	}
	return caskroom.Cask{}, false
}

func (e CaskEnv) isOutdated(ic caskroom.Cask) bool {
	r, err := outdated.CheckCasks(e.Casks, e.Prefix, []caskroom.Cask{ic}, e.Outdated)
	return err == nil && len(r) > 0
}

// installedRows lists installs of the cask and its versioned relatives (1password@nightly) by full name.
func (e CaskEnv) installedRows(c api.Cask) []string {
	base, _, _ := strings.Cut(c.Token, "@")
	type row struct{ name, version, size, warning string }
	var rows []row
	for _, ic := range e.Installed {
		if ic.Token != base && !strings.HasPrefix(ic.Token, base+"@") {
			continue
		}
		r := row{name: ic.Token, version: ic.Version}
		if ic.Tap != "" && ic.Tap != "homebrew/cask" {
			r.name = ic.Tap + "/" + ic.Token
		}
		if current, ok := e.Casks.Lookup(ic.Token); ok && e.isOutdated(ic) {
			if here, err := current.ForTag(e.Outdated.Tag); err == nil && here.Version != ic.Version {
				r.version += " → " + here.Version
			}
		}
		if size, ok := caskroom.DiskUsage(e.Prefix, ic.Token, ic.Version); ok {
			r.size = "(" + cellar.ReadableSize(size) + ")"
		} else {
			r.warning = filepath.Join(e.Prefix, "Caskroom", ic.Token, ic.Version) + " (does not exist)"
		}
		rows = append(rows, r)
	}
	slices.SortFunc(rows, func(a, b row) int { return strings.Compare(a.name, b.name) })

	nameWidth, versionWidth := 0, 0
	for _, r := range rows {
		nameWidth = max(nameWidth, len([]rune(r.name)))
		versionWidth = max(versionWidth, len([]rune(r.version)))
	}
	pad := func(s string, n int) string { return s + strings.Repeat(" ", n-len([]rune(s))) }
	var out []string
	for _, r := range rows {
		line := pad(r.name, nameWidth) + " " + pad(r.version, versionWidth)
		if r.size != "" {
			line += " " + r.size
		}
		out = append(out, line)
		if r.warning != "" {
			out = append(out, "  "+r.warning)
		}
	}
	return out
}

// caskDependencies renders the Dependencies block, with a count of installed runtime dependencies.
func (e CaskEnv) caskDependencies(c api.Cask, tag string) string {
	all := slices.Clone(c.DependsOn.Formula)
	for _, d := range c.DependsOn.Cask {
		all = append(all, d+" (cask)")
	}
	if len(all) == 0 {
		return ""
	}
	formulae, casks := map[string]bool{}, map[string]bool{}
	e.collectDependencies(c, tag, formulae, casks, map[string]bool{c.Token: true})
	lines := []string{fmt.Sprintf("Required (%d): %s", len(all), strings.Join(all, ", "))}
	if total := len(formulae) + len(casks); total > 0 {
		installed := 0
		for name := range formulae {
			if len(e.kegsOf(name)) > 0 {
				installed++
			}
		}
		for token := range casks {
			if fi, err := os.Stat(filepath.Join(e.Prefix, "Caskroom", token)); err == nil && fi.IsDir() {
				installed++
			}
		}
		lines = append(lines, fmt.Sprintf("Recursive Runtime (%d): %s", total, dependencyStatus(installed, total)))
	}
	return "==> Dependencies\n" + strings.Join(lines, "\n") + "\n"
}

// collectDependencies follows brew's collect_cask_dependency_names: a formula
// brings the runtime dependencies its installed keg recorded, a cask its own
// dependencies.
func (e CaskEnv) collectDependencies(c api.Cask, tag string, formulae, casks, visited map[string]bool) {
	for _, name := range c.DependsOn.Formula {
		formulae[name] = true
		if kegs := e.kegsOf(name); len(kegs) > 0 {
			if k := cellar.Prefix(e.Prefix).DefaultKeg(kegs); k.Receipt != nil {
				for _, d := range k.Receipt.RuntimeDependencies {
					formulae[d.FullName] = true
				}
			}
		}
	}
	for _, token := range c.DependsOn.Cask {
		if visited[token] {
			continue
		}
		casks[token], visited[token] = true, true
		if dep, ok := e.Casks.Lookup(token); ok {
			if here, err := dep.ForTag(tag); err == nil {
				e.collectDependencies(here, tag, formulae, casks, visited)
			}
		}
	}
}

// kegsOf lists the installed kegs of a formula, named in full or not.
func (e CaskEnv) kegsOf(fullName string) []cellar.Keg {
	name := fullName[strings.LastIndex(fullName, "/")+1:]
	var out []cellar.Keg
	for _, k := range e.Kegs {
		if k.Name == name {
			out = append(out, k)
		}
	}
	return out
}

func caskRequirements(c api.Cask) string {
	var reqs []string
	for _, a := range c.DependsOn.Arch {
		arch := a.Type
		switch {
		case a.Bits == 64 && a.Type == "intel":
			arch = "x86_64"
		case a.Bits == 64:
			arch = a.Type + "64"
		case a.Bits == 32 && a.Type == "intel":
			arch = "i386"
		}
		reqs = append(reqs, arch+" architecture")
	}
	if m := c.DependsOn.MacOS; m != nil {
		if len(m.Versions) == 0 {
			reqs = append(reqs, "macOS")
		} else {
			reqs = append(reqs, "macOS "+m.Comparator+" "+strings.Join(m.Versions, " / "))
		}
	}
	if len(reqs) == 0 {
		return ""
	}
	return "==> Requirements\nRequired: " + strings.Join(reqs, ", ") + "\n"
}

// artifactKind is how brew info shows one kind of artifact; kinds it omits (binaries, uninstall) are absent.
type artifactKind struct {
	order     int // brew's install order, which sorts the artifacts
	heading   string
	name      string // brew's English name, shown after the summary under Plugins and Artifacts
	relocated bool   // summarised as the source and any target
}

var artifactKinds = map[string]artifactKind{
	"generated_script":                     {4, "Artifacts", "Generated Script", false},
	"installer":                            {5, "Artifacts", "Installer", false},
	"pkg":                                  {6, "Artifacts", "Pkg", false},
	"app":                                  {7, "Apps", "App", true},
	"app_image":                            {7, "Apps", "App Image", true},
	"suite":                                {7, "Artifacts", "App Suite", true},
	"artifact":                             {7, "Artifacts", "Generic Artifact", true},
	"colorpicker":                          {7, "Plugins", "Colorpicker", true},
	"prefpane":                             {7, "Artifacts", "Preference Pane", true},
	"qlplugin":                             {7, "Plugins", "Quick Look Plugin", true},
	"mdimporter":                           {7, "Artifacts", "Spotlight metadata importer", true},
	"dictionary":                           {7, "Artifacts", "Dictionary", true},
	"font":                                 {7, "Fonts", "Font", true},
	"service":                              {7, "Artifacts", "Service", true},
	"input_method":                         {7, "Artifacts", "Input Method", true},
	"internet_plugin":                      {7, "Plugins", "Internet Plugin", true},
	"keyboard_layout":                      {7, "Artifacts", "Keyboard Layout", true},
	"audio_unit_plugin":                    {7, "Plugins", "Audio Unit Plugin", true},
	"vst_plugin":                           {7, "Plugins", "VST Plugin", true},
	"vst3_plugin":                          {7, "Plugins", "VST3 Plugin", true},
	"screen_saver":                         {7, "Artifacts", "Screen Saver", true},
	"manpage":                              {9, "Manpages", "Manpage", true},
	"bash_completion":                      {10, "Artifacts", "Bash Completion", true},
	"zsh_completion":                       {10, "Artifacts", "Zsh Completion", true},
	"fish_completion":                      {10, "Artifacts", "Fish Completion", true},
	"pwsh_completion":                      {10, "Artifacts", "Pwsh Completion", true},
	"generate_completions_from_executable": {11, "Artifacts", "Generated Completion", false},
}

// artifactSections renders the artifact blocks. Brew's sort by kind is unstable, so order within a kind may differ.
func artifactSections(c api.Cask, fill func(string) string) string {
	type entry struct {
		kind   artifactKind
		detail string
	}
	var entries []entry
	for _, raw := range c.Artifacts {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			continue
		}
		for key, args := range obj {
			kind, ok := artifactKinds[key]
			if !ok {
				continue
			}
			var list []json.RawMessage
			if json.Unmarshal(args, &list) != nil || len(list) == 0 {
				continue
			}
			summary := summarize(key, kind, list, c.Token)
			if summary == "" {
				continue
			}
			summary = fill(summary)
			if kind.heading == "Plugins" || kind.heading == "Artifacts" {
				summary += " (" + kind.name + ")"
			}
			entries = append(entries, entry{kind, summary})
		}
	}
	slices.SortStableFunc(entries, func(a, b entry) int { return a.kind.order - b.kind.order })

	var sections []string
	for _, heading := range []string{"Apps", "Fonts", "Manpages", "Plugins", "Artifacts"} {
		var lines []string
		for _, e := range entries {
			if e.kind.heading == heading {
				lines = append(lines, e.detail)
			}
		}
		if len(lines) > 0 {
			sections = append(sections, "==> "+heading+"\n"+strings.Join(lines, "\n"))
		}
	}
	return strings.Join(sections, "\n")
}

// summarize is brew's one-line summary of an artifact, from its API arguments.
func summarize(key string, kind artifactKind, list []json.RawMessage, token string) string {
	var first string
	firstIsString := json.Unmarshal(list[0], &first) == nil
	switch {
	case kind.relocated:
		if !firstIsString {
			return ""
		}
		if len(list) > 1 {
			var opts struct {
				Target string `json:"target"`
			}
			if json.Unmarshal(list[1], &opts) == nil && opts.Target != "" {
				return first + " -> " + opts.Target
			}
		}
		return first
	case key == "installer":
		var opts struct {
			Manual string `json:"manual"`
			Script struct {
				Executable string `json:"executable"`
			} `json:"script"`
		}
		if json.Unmarshal(list[0], &opts) != nil {
			return ""
		}
		return cmpOr(opts.Manual, opts.Script.Executable)
	case key == "generate_completions_from_executable":
		var commands []string
		opts := struct {
			BaseName string   `json:"base_name"`
			Shells   []string `json:"shells"`
		}{Shells: []string{"bash", "zsh", "fish"}}
		for _, a := range list {
			var s string
			if json.Unmarshal(a, &s) == nil {
				commands = append(commands, s)
			} else {
				_ = json.Unmarshal(a, &opts)
			}
		}
		if len(commands) == 0 {
			return ""
		}
		base := cmpOr(opts.BaseName, filepath.Base(commands[0]), token)
		return fmt.Sprintf("%s (base_name: %s, shells: %s)", strings.Join(commands, " "), base, strings.Join(opts.Shells, ", "))
	}
	if firstIsString { // pkg, generated_script
		return first
	}
	return ""
}

func cmpOr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// placeholders fills in the API's placeholders as brew does when it loads a cask.
func (e CaskEnv) placeholders(appDir string) func(string) string {
	return cask.Placeholders(cask.Where{Prefix: e.Prefix, AppDir: appDir, Home: e.Home})
}

// caskCaveats adds brew's Rosetta caveat on Apple silicon without Rosetta 2.
func (e CaskEnv) caskCaveats(c api.Cask, appDir string) string {
	var parts []string
	if c.Caveats != "" {
		parts = append(parts, e.placeholders(appDir)(c.Caveats))
	}
	if c.CaveatsRosetta && e.Host.OS == "darwin" && e.Host.Arch == "arm64" && !e.Rosetta {
		parts = append(parts, c.Token+` is built for Intel macOS and so requires Rosetta 2 to be installed.
You can install Rosetta 2 with:
  softwareupdate --install-rosetta --agree-to-license
Note that it is very difficult to remove Rosetta 2 once it is installed.
`)
	}
	return strings.Join(parts, "\n")
}

// CaskDeprecation is the notice for a deprecated or disabled cask, or "".
func CaskDeprecation(c *api.Cask, now time.Time) string {
	var kind, reason string
	switch {
	case c.Deprecated:
		kind, reason = "Deprecated", c.DeprecationReason
	case c.Disabled:
		kind, reason = "Disabled", c.DisableReason
	default:
		return ""
	}
	// Brew names the kind deprecated first but takes a disabled cask's replacement first.
	replacement := replacementWithType(c.DeprecationReplacementFormula, c.DeprecationReplacementCask)
	if c.Disabled {
		replacement = replacementWithType(c.DisableReplacementFormula, c.DisableReplacementCask)
	}
	if r, ok := cask.DeprecationReasons[reason]; ok {
		reason = r
	}
	msg := kind + "!"
	if reason != "" {
		msg = kind + " because it " + reason + "!"
	}
	today := now.Format(time.DateOnly)
	disableDate := c.DisableDate
	if disableDate == "" && c.DeprecationDate != "" {
		// Brew only shows an estimate that has not passed.
		if d, err := time.Parse(time.DateOnly, c.DeprecationDate); err == nil {
			if est := plusYear(d).Format(time.DateOnly); est >= today {
				disableDate = est
			}
		}
	}
	if disableDate != "" {
		if disableDate < today {
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
