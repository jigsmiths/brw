// Package services starts and stops the launchd agents a keg ships a plist for,
// following brew's Library/Homebrew/services.
package services

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	ErrNotLoaded = errors.New("service is not loaded")
	ErrNoService = errors.New("keg has no service")
)

const (
	StatusESRCH              = 3
	StatusEINPROGRESS        = 36
	StatusDomainNotSupported = 125
)

const (
	canonicalPrefix = "sh.brew."
	legacyPrefix    = "homebrew.mxcl."
)

// brew sizes the status column for the color codes of its longest cell.
const (
	tableMinStatusWidth = 15
	tableColorWidth     = 9
	tableMinNameWidth   = 4
	tableMinUserWidth   = 4
)

// Launchctl is the part of launchctl brw drives. Print and List return
// ErrNotLoaded when launchd has no such service.
type Launchctl interface {
	Print(domain, label string) (string, error)
	List(label string) (string, error)
	Enable(domain, label string) error
	Bootstrap(domain, file string) error
	Bootout(domain, label string) error
	Stop(label string) error
}

func Names(formula string) []string {
	return []string{canonicalPrefix + formula, legacyPrefix + formula}
}

func Domain(uid int) string { return "gui/" + strconv.Itoa(uid) }

// Domains are the launchd domains brew probes for a user, in order.
func Domains(uid int) []string {
	return []string{Domain(uid), "user/" + strconv.Itoa(uid)}
}

func ParseEnvFile(content string) (env map[string]string, skipped []string) {
	env = map[string]string{}
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if key == "" || !found {
			skipped = append(skipped, line)
			continue
		}
		env[key] = strings.TrimSpace(value)
	}
	return env, skipped
}

var (
	envDict      = regexp.MustCompile(`(?s)\t<key>EnvironmentVariables</key>\n\t<dict>\n(.*?)\t</dict>\n`)
	envEntry     = regexp.MustCompile(`\t\t<key>([^<]*)</key>\n\t\t<string>([^<]*)</string>\n`)
	topLevelKey  = regexp.MustCompile(`(?m)^\t<key>([^<]+)</key>$`)
	xmlEscaper   = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	plistTail    = "</dict>\n</plist>\n"
	envDictKey   = "EnvironmentVariables"
	envDictStart = "\t<key>" + envDictKey + "</key>\n\t<dict>\n"
)

// ApplyEnv sets variables in a plist's EnvironmentVariables as brew's
// effective_environment_variables does: overrides win, keys stay sorted.
// False means the plist had no shape it could edit.
func ApplyEnv(plist []byte, env map[string]string) ([]byte, bool) {
	s := string(plist)
	if !strings.HasSuffix(s, plistTail) || !strings.Contains(s, "\n<dict>\n") {
		return plist, false
	}
	if len(env) == 0 {
		return plist, true
	}
	vars := map[string]string{}
	loc := envDict.FindStringSubmatchIndex(s)
	if loc != nil {
		for _, m := range envEntry.FindAllStringSubmatch(s[loc[2]:loc[3]], -1) {
			vars[m[1]] = m[2]
		}
	}
	for k, v := range env {
		vars[xmlEscaper.Replace(k)] = xmlEscaper.Replace(v)
	}
	var b strings.Builder
	b.WriteString(envDictStart)
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", k, vars[k])
	}
	b.WriteString("\t</dict>\n")
	if loc != nil {
		return []byte(s[:loc[0]] + b.String() + s[loc[1]:]), true
	}
	at := len(s) - len(plistTail)
	for _, m := range topLevelKey.FindAllStringSubmatchIndex(s, -1) {
		if s[m[2]:m[3]] > envDictKey {
			at = m[0]
			break
		}
	}
	return []byte(s[:at] + b.String() + s[at:]), true
}

func FindPlist(kegPath string, names []string) (string, error) {
	for _, n := range names {
		p := filepath.Join(kegPath, n+".plist")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	matches, err := filepath.Glob(filepath.Join(kegPath, "*.plist"))
	if err != nil {
		return "", err
	}
	if len(matches) > 0 {
		return matches[0], nil
	}
	return "", ErrNoService
}

var (
	labelKey    = regexp.MustCompile(`<key>Label</key>\s*<string>([^<]+)</string>`)
	userNameKey = regexp.MustCompile(`<key>UserName</key>\s*<string>([^<]+)</string>`)
)

func FileLabel(plist []byte) (string, bool) { return firstString(labelKey, plist) }

func FileUser(plist []byte) (string, bool) { return firstString(userNameKey, plist) }

func firstString(re *regexp.Regexp, b []byte) (string, bool) {
	m := re.FindSubmatch(b)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}

func Timed(plist []byte) bool {
	s := string(plist)
	return strings.Contains(s, "<key>StartInterval</key>") || strings.Contains(s, "<key>StartCalendarInterval</key>")
}

type Info struct {
	Blank       bool
	PID         int
	ExitCode    int
	HasExitCode bool
}

func (i Info) Running() bool { return i.PID > 0 }

var (
	printPID  = regexp.MustCompile(`pid = ([0-9]+)`)
	printExit = regexp.MustCompile(`last exit code = ([0-9]+)`)
	listPID   = regexp.MustCompile(`"PID" = ([0-9]*);`)
	listExit  = regexp.MustCompile(`"LastExitStatus" = ([0-9]*);`)
)

func ParsePrint(out string) Info { return parse(out, printPID, printExit) }

func ParseList(out string) Info { return parse(out, listPID, listExit) }

func parse(out string, pid, exit *regexp.Regexp) Info {
	i := Info{Blank: strings.TrimSpace(out) == ""}
	if m := pid.FindStringSubmatch(out); m != nil {
		i.PID, _ = strconv.Atoi(m[1])
	}
	if m := exit.FindStringSubmatch(out); m != nil {
		i.ExitCode, _ = strconv.Atoi(m[1])
		i.HasExitCode = true
	}
	return i
}

type State struct {
	Loaded bool
	Timed  bool
	Info   Info
}

// Classify is brew's FormulaWrapper#to_hash status, in its order.
func Classify(s State) string {
	switch {
	case s.Info.Running():
		return "started"
	case !s.Loaded:
		return "none"
	case s.Info.HasExitCode && s.Info.ExitCode == 0 && s.Timed:
		return "scheduled"
	case s.Info.HasExitCode && s.Info.ExitCode == 0:
		return "stopped"
	case s.Info.HasExitCode:
		return "error"
	case s.Info.Blank:
		return "unknown"
	}
	return "other"
}

func Query(l Launchctl, domains []string, label string) (loaded bool, info Info, err error) {
	for _, d := range domains {
		out, err := l.Print(d, label)
		if err == nil && strings.TrimSpace(out) != "" {
			return true, ParsePrint(out), nil
		}
		if err != nil && !errors.Is(err, ErrNotLoaded) {
			return false, Info{}, err
		}
	}
	out, err := l.List(label)
	if errors.Is(err, ErrNotLoaded) || (err == nil && strings.TrimSpace(out) == "") {
		return false, Info{}, nil
	}
	if err != nil {
		return false, Info{}, err
	}
	return true, ParseList(out), nil
}

type Row struct {
	Name   string  `json:"name"`
	Status string  `json:"status"`
	User   *string `json:"user"`
	File   string  `json:"file"`
	Exit   *int    `json:"exit_code"`

	Loaded bool `json:"-"`
}

type palette struct{ green, plain, red, yellow, bold, reset string }

func paletteFor(color bool) palette {
	if !color {
		return palette{}
	}
	return palette{green: "\x1b[32m", plain: "\x1b[39m", red: "\x1b[31m", yellow: "\x1b[33m", bold: "\x1b[1m", reset: "\x1b[0m"}
}

func (p palette) status(r Row) string {
	switch r.Status {
	case "started", "scheduled":
		return p.green + r.Status + p.reset
	case "stopped", "none":
		return p.plain + r.Status + p.reset
	case "error":
		code := ""
		if r.Exit != nil {
			code = strconv.Itoa(*r.Exit)
		}
		return p.red + "error  " + p.reset + code
	}
	return p.yellow + r.Status + p.reset
}

// FormatTable lays out rows as brew's print_table does, padding the status
// column by the length of its color codes too, as brew does.
func FormatTable(rows []Row, color bool, home string) string {
	p := paletteFor(color)
	nameW, statusW, userW := tableMinNameWidth, tableMinStatusWidth, tableMinUserWidth
	statuses := make([]string, len(rows))
	for i, r := range rows {
		statuses[i] = p.status(r)
		nameW = max(nameW, len(r.Name))
		statusW = max(statusW, len(statuses[i]))
		if r.User != nil {
			userW = max(userW, len(*r.User))
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s%-*s %-*s %-*s %s%s\n", p.bold, nameW, "Name", statusW-tableColorWidth, "Status", userW, "User", "File", p.reset)
	for i, r := range rows {
		user, file := "", ""
		if r.User != nil {
			user = *r.User
		}
		if r.Loaded && r.File != "" {
			file = r.File
			if home != "" {
				file = strings.ReplaceAll(file, home, "~")
			}
		}
		fmt.Fprintf(&b, "%-*s %-*s %-*s %s\n", nameW, r.Name, statusW, statuses[i], userW, user, file)
	}
	return b.String()
}

func FormatJSON(rows []Row) (string, error) {
	if len(rows) == 0 {
		return "[]\n", nil
	}
	out, err := json.MarshalIndent(rows, "", "  ")
	return string(out) + "\n", err
}

func AgentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}
