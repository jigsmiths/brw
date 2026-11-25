package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jigsmiths/brw/internal/services"
)

const (
	ansiGreen = "\x1b[32m"
	ansiRed   = "\x1b[31m"
)

type infoCron struct {
	Month   any `json:"Month"`
	Day     any `json:"Day"`
	Weekday any `json:"Weekday"`
	Hour    any `json:"Hour"`
	Minute  any `json:"Minute"`
}

func (c infoCron) inspect() string {
	field := func(name string, v any) string {
		if s, ok := v.(string); ok {
			return name + ": " + strconv.Quote(s)
		}
		return fmt.Sprintf("%s: %v", name, v)
	}
	return "{" + strings.Join([]string{
		field("Month", c.Month), field("Day", c.Day), field("Weekday", c.Weekday),
		field("Hour", c.Hour), field("Minute", c.Minute),
	}, ", ") + "}"
}

type infoBlock struct {
	Command      string    `json:"command"`
	WorkingDir   *string   `json:"working_dir"`
	RootDir      *string   `json:"root_dir"`
	LogPath      *string   `json:"log_path"`
	ErrorLogPath *string   `json:"error_log_path"`
	Interval     *int      `json:"interval"`
	Cron         *infoCron `json:"cron"`
}

type serviceInfo struct {
	Name        string  `json:"name"`
	ServiceName string  `json:"service_name"`
	Running     bool    `json:"running"`
	Loaded      bool    `json:"loaded"`
	Schedulable bool    `json:"schedulable"`
	PID         *int    `json:"pid"`
	ExitCode    *int    `json:"exit_code"`
	User        *string `json:"user"`
	Status      string  `json:"status"`
	File        string  `json:"file"`
	Registered  bool    `json:"registered"`
	LoadedFile  *string `json:"loaded_file"`
	*infoBlock
}

func (e *env) servicesInfo(targets []serviceTarget, o *servicesOpts, prefix string) error {
	infos := make([]serviceInfo, 0, len(targets))
	for _, t := range targets {
		info, err := e.serviceInfo(t, prefix)
		if err != nil {
			return err
		}
		infos = append(infos, info)
	}
	if o.json {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(infos); err != nil {
			return err
		}
		_, err := fmt.Fprint(e.out, buf.String())
		return err
	}
	for _, info := range infos {
		_, _ = fmt.Fprint(e.out, e.infoText(info, o.verbose))
	}
	return nil
}

func (e *env) serviceInfo(t serviceTarget, prefix string) (serviceInfo, error) {
	s, err := e.snapshot(t)
	if err != nil {
		return serviceInfo{}, err
	}
	timed := t.block.Timed
	if !t.block.Defined {
		if body, err := os.ReadFile(t.source); err == nil {
			timed = services.Timed(body)
		}
	}
	info := serviceInfo{
		Name:        t.name,
		ServiceName: s.active,
		Running:     s.info.Running(),
		Loaded:      s.loaded,
		Schedulable: timed,
		User:        e.owner(t, s),
		Status:      services.Classify(services.State{Loaded: s.loaded, Timed: timed, Info: s.info}),
		File:        e.serviceFile(t, s, prefix),
		Registered:  e.registeredInEitherDir(t),
	}
	if s.info.HasPID {
		info.PID = &s.info.PID
	}
	if s.info.HasExitCode {
		info.ExitCode = &s.info.ExitCode
	}
	if s.info.HasLoadedFile {
		info.LoadedFile = &s.info.LoadedFile
	}
	if t.block.Defined && len(t.block.Command) > 0 {
		info.infoBlock = e.infoBlock(t)
	}
	return info, nil
}

func (e *env) infoBlock(t serviceTarget) *infoBlock {
	b := &infoBlock{Command: e.manualCommand(t)}
	field := func(name string) *string {
		if v, ok := t.block.Fields[name]; ok {
			return &v
		}
		return nil
	}
	b.WorkingDir, b.RootDir = field("working_dir"), field("root_dir")
	b.LogPath, b.ErrorLogPath = field("log_path"), field("error_log_path")
	if v := field("interval"); v != nil {
		if n, err := strconv.Atoi(*v); err == nil {
			b.Interval = &n
		}
	}
	if len(t.block.Cron) > 0 {
		value := func(i int) any {
			if n, err := strconv.Atoi(t.block.Cron[i][1]); err == nil {
				return n
			}
			return t.block.Cron[i][1]
		}
		b.Cron = &infoCron{Month: value(0), Day: value(1), Weekday: value(2), Hour: value(3), Minute: value(4)}
	}
	return b
}

func (e *env) manualCommand(t serviceTarget) string {
	vars := slices.Clone(t.block.Env)
	overrides, _ := e.userEnv(t.name)
	for _, kv := range overrides {
		if i := slices.IndexFunc(vars, func(v [2]string) bool { return v[0] == kv[0] }); i >= 0 {
			vars[i][1] = kv[1]
			continue
		}
		vars = append(vars, kv)
	}
	var parts []string
	for _, kv := range vars {
		if kv[0] != "PATH" {
			parts = append(parts, kv[0]+"=\""+kv[1]+"\"")
		}
	}
	for _, arg := range t.block.Command {
		parts = append(parts, shQuote(arg))
	}
	return strings.Join(parts, " ")
}

var shellUnsafe = regexp.MustCompile(`[^A-Za-z0-9_\-.,:+/@\n]`)

func shQuote(s string) string {
	if s == "" {
		return "''"
	}
	s = shellUnsafe.ReplaceAllString(s, `\${0}`)
	s = strings.ReplaceAll(s, "\n", "'\n'")
	return strings.ReplaceAll(s, `\~`, "~")
}

func (e *env) prettyBool(b bool) string {
	if !e.tty || os.Getenv("HOMEBREW_NO_EMOJI") != "" {
		return strconv.FormatBool(b)
	}
	if b {
		return ansiBold + ansiGreen + "✔" + ansiReset + ansiReset
	}
	return ansiBold + ansiRed + "✘" + ansiReset + ansiReset
}

func (e *env) infoText(info serviceInfo, verbose bool) string {
	bold, reset := "", ""
	if e.tty {
		bold, reset = ansiBold, ansiReset
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s%s (%s)\n", bold, info.Name, reset, info.ServiceName)
	fmt.Fprintf(&b, "Running: %s\nLoaded: %s\nSchedulable: %s\n",
		e.prettyBool(info.Running), e.prettyBool(info.Loaded), e.prettyBool(info.Schedulable))
	if info.PID != nil {
		user := ""
		if info.User != nil {
			user = *info.User
		}
		fmt.Fprintf(&b, "User: %s\nPID: %d\n", user, *info.PID)
	}
	if !verbose {
		return b.String()
	}
	fmt.Fprintf(&b, "File: %s %s\nRegistered at login: %s\n", info.File, e.prettyBool(info.File != ""), e.prettyBool(info.Registered))
	if info.infoBlock == nil {
		return b.String()
	}
	fmt.Fprintf(&b, "Command: %s\n", info.Command)
	show := func(label string, value *string) {
		if value != nil {
			fmt.Fprintf(&b, "%s: %s\n", label, *value)
		}
	}
	show("Working directory", info.WorkingDir)
	show("Root directory", info.RootDir)
	show("Log", info.LogPath)
	show("Error log", info.ErrorLogPath)
	if info.Interval != nil {
		fmt.Fprintf(&b, "Interval: %ds\n", *info.Interval)
	}
	if info.Cron != nil {
		fmt.Fprintf(&b, "Cron: %s\n", info.Cron.inspect())
	}
	return b.String()
}

func (e *env) serviceFileBase(t serviceTarget) string {
	if t.source == "" {
		return t.serviceName() + ".plist"
	}
	return filepath.Base(t.source)
}
