// Package services starts and stops the launchd agents a keg ships a plist for,
// following brew's Library/Homebrew/services.
package services

import (
	"encoding/json"
	"errors"
	"fmt"
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
	Jobs() (string, error)
	Enable(domain, label string) error
	Bootstrap(domain, file string) error
	Bootout(domain, label string) error
	Stop(label string) error
}

func Names(formula string) []string {
	return []string{canonicalPrefix + formula, legacyPrefix + formula}
}

const systemDomain = "system"

func Domain(uid int) string {
	if uid == 0 {
		return systemDomain
	}
	return "gui/" + strconv.Itoa(uid)
}

func Domains(uid int) []string { return Candidates(Domain(uid), uid) }

func Candidates(chosen string, uid int) []string {
	if uid == 0 {
		return []string{systemDomain}
	}
	var out []string
	for _, d := range []string{chosen, UserDomain(uid), Domain(uid)} {
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

func UserDomain(uid int) string { return "user/" + strconv.Itoa(uid) }

func ToF(s string) float64 { return rubyToF(s) }

func ParseEnvFile(content string) (env map[string]string, skipped []string) {
	pairs, skipped := ParseEnvPairs(content)
	env = map[string]string{}
	for _, kv := range pairs {
		env[kv[0]] = kv[1]
	}
	return env, skipped
}

func ParseEnvPairs(content string) (pairs [][2]string, skipped []string) {
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
		value = strings.TrimSpace(value)
		if i := slices.IndexFunc(pairs, func(kv [2]string) bool { return kv[0] == key }); i >= 0 {
			pairs[i][1] = value
			continue
		}
		pairs = append(pairs, [2]string{key, value})
	}
	return pairs, skipped
}

// Program is how take_root_ownership? finds the executable. parsed is false when
// the plist does not parse, where brew gives up on the rest of the ownership change.
func Program(plist []byte) (path, source string, parsed bool) {
	root, err := parsePlist(string(plist))
	if err != nil {
		return "", "", false
	}
	if _, isNil := root.(plistNil); isNil {
		return "", "", false
	}
	dict, _ := root.(*plistDict)
	if dict == nil {
		return "", "", true
	}
	value := func(key string) (string, bool) {
		for _, e := range dict.entries {
			if e.id == "s:"+key {
				s, ok := e.val.(string)
				return s, ok
			}
		}
		return "", false
	}
	for _, e := range dict.entries {
		if args, ok := e.val.([]any); ok && e.id == "s:ProgramArguments" && len(args) > 0 {
			if first, ok := args[0].(string); ok && strings.TrimSpace(first) != "" {
				return first, "first ProgramArguments value", true
			}
		}
	}
	if program, ok := value("Program"); ok && strings.TrimSpace(program) != "" {
		return program, "Program", true
	}
	return "", "", true
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

func FileLabel(plist []byte) (string, bool) { return plistString(plist, "Label") }

func FileUser(plist []byte) (string, bool) { return plistString(plist, userNameKeyName) }

// plistString is `Plist.parse_xml(...)[key]` kept when it is a non-blank String; a plist that does not parse has none.
func plistString(plist []byte, key string) (string, bool) {
	root, err := parsePlist(string(plist))
	if err != nil {
		return "", false
	}
	var value any
	switch x := root.(type) {
	case *plistDict:
		for _, e := range x.entries {
			if e.id == "s:"+key {
				value = e.val
			}
		}
	case string:
		if strings.Contains(x, key) {
			value = key
		}
	}
	s, ok := value.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", false
	}
	return s, true
}

type Info struct {
	Blank         bool
	PID           int
	HasPID        bool
	ExitCode      int
	HasExitCode   bool
	LoadedFile    string
	HasLoadedFile bool
}

func (i Info) Running() bool { return i.PID > 0 }

var (
	printPID  = regexp.MustCompile(`pid = ([0-9]+)`)
	printExit = regexp.MustCompile(`last exit code = ([0-9]+)`)
	listPID   = regexp.MustCompile(`"PID" = ([0-9]*);`)
	listExit  = regexp.MustCompile(`"LastExitStatus" = ([0-9]*);`)
	printPath = regexp.MustCompile(`path = (.*)`)
)

func ParsePrint(out string) Info {
	i := parse(out, printPID, printExit)
	if m := printPath.FindStringSubmatch(out); m != nil {
		i.LoadedFile, i.HasLoadedFile = m[1], true
	}
	return i
}

func ParseList(out string) Info { return parse(out, listPID, listExit) }

func parse(out string, pid, exit *regexp.Regexp) Info {
	i := Info{Blank: strings.TrimSpace(out) == ""}
	if m := pid.FindStringSubmatch(out); m != nil {
		i.PID, _ = strconv.Atoi(m[1])
		i.HasPID = true
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
