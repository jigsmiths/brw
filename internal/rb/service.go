package rb

import (
	"fmt"
	"maps"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ServiceFiles are the launchd plist and systemd unit files brew's install_service writes, by file name.
func (f *Formula) ServiceFiles(root string) (files map[string]string, err error) {
	if f.b.serviceBlk == nil {
		return nil, nil
	}
	defer catch(f.b.name, &err)
	s := f.renderService(root)
	cmd := s.command()
	if len(cmd) == 0 {
		return nil, nil
	}
	plist, unit := s.names()
	files = map[string]string{plist + ".plist": s.plist(cmd, plist), unit + ".service": s.unit(cmd)}
	if s.timed() {
		files[unit+".timer"] = s.timer(unit)
	}
	return files, nil
}

func (f *Formula) ServiceRequiresRoot(root string) (requires bool, err error) {
	if f.b.serviceBlk == nil {
		return false, nil
	}
	defer catch(f.b.name, &err)
	return f.renderService(root).get("require_root") == rbool(true), nil
}

func (f *Formula) renderService(root string) renderedService {
	sv := &serviceB{f: f.b}
	in := &interp{plat: f.b.plat, ctx: []dsl{&instanceDSL{f: f.b, root: root, linked: true}, sv}}
	in.run(f.b.serviceBlk.body, &scope{vars: map[string]value{}, parent: f.b.serviceScope})
	return renderedService{sv: sv, root: root, os: f.b.plat.real().os}
}

func catch(name string, err *error) {
	r := recover()
	switch x := r.(type) {
	case nil:
	case unsupported:
		*err = fmt.Errorf("%s: %w", name, x)
	case buildFailure:
		*err = fmt.Errorf("%s: %w", name, x.err)
	case parseErr:
		*err = fmt.Errorf("%s: %w: %s", name, ErrUnsupported, x.msg)
	default:
		panic(r)
	}
}

type renderedService struct {
	sv   *serviceB
	root string
	os   string
}

func (s renderedService) get(k string) value {
	if s.sv.fields == nil {
		return nil
	}
	v, _ := s.sv.fields.get(rstr(k))
	return v
}

func (s renderedService) str(v value) string {
	return strings.ReplaceAll(str(v), PrefixPlaceholder, s.root)
}

func expandOrFail(p string) string {
	out, err := ExpandPath(p)
	if err != nil {
		fail("%v", err)
	}
	return out
}

// ExpandPath is File.expand_path: ~ and ~user are replaced and the result is absolute and clean.
func ExpandPath(p string) (string, error) {
	if strings.HasPrefix(p, "~") {
		name, rest, _ := strings.Cut(p[1:], "/")
		if rest != "" || strings.HasSuffix(p, "/") {
			rest = "/" + rest
		}
		home := ""
		if name == "" {
			var ok bool
			if home, ok = lookupEnv("HOME"); !ok {
				home, _ = os.UserHomeDir()
			}
		} else {
			var ok bool
			if home, ok = lookupHome(name); !ok {
				return "", fmt.Errorf("user %s doesn't exist", name)
			}
		}
		p = home + rest
	}
	if !filepath.IsAbs(p) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		p = filepath.Join(wd, p)
	}
	return filepath.Clean(p), nil
}

var lookupHome = func(name string) (string, bool) {
	u, err := user.Lookup(name)
	if err != nil {
		return "", false
	}
	return u.HomeDir, true
}

func (s renderedService) arg(v value) string {
	out := s.str(v)
	if strings.HasPrefix(out, "~") {
		return expandOrFail(out)
	}
	return out
}

func (s renderedService) path(k string) (string, bool) {
	v := s.get(k)
	if blank(v) {
		return "", false
	}
	return expandOrFail(s.str(v)), true
}

func (s renderedService) command() []string {
	run := s.get("run")
	if h, ok := run.(*hash); ok {
		run, _ = h.get(rstr(s.os))
	}
	var out []string
	switch x := run.(type) {
	case *arr:
		for _, e := range x.v {
			out = append(out, s.arg(e))
		}
	case nil:
	default:
		out = []string{s.arg(x)}
	}
	return out
}

func (s renderedService) names() (plist, unit string) {
	plist, unit = "sh.brew."+s.sv.f.name, "sh.brew."+s.sv.f.name
	if h, ok := s.get("name").(*hash); ok {
		if v, ok := h.get(rstr("macos")); ok && v != nil {
			plist = str(v)
		}
		if v, ok := h.get(rstr("linux")); ok && v != nil {
			unit = str(v)
		}
	}
	return plist, unit
}

func (s renderedService) runType() string {
	if v := s.get("run_type"); v != nil {
		return str(v)
	}
	return "immediate"
}

func (s renderedService) timed() bool { return s.runType() == "cron" || s.runType() == "interval" }

func (s renderedService) keepAlive() *hash {
	h, _ := s.get("keep_alive").(*hash)
	if h == nil {
		return &hash{}
	}
	return h
}

func kwVal(h *hash, k string) (value, bool) {
	if v, ok := h.get(sym(k)); ok {
		return v, true
	}
	return h.get(rstr(k))
}

func (s renderedService) env() [][2]string {
	h, _ := s.get("environment_variables").(*hash)
	if h == nil {
		return nil
	}
	var out [][2]string
	for i, k := range h.keys {
		out = append(out, [2]string{str(k), s.str(h.vals[i])})
	}
	return out
}

func (s renderedService) cron() [][2]string {
	if s.sv.cron == "" {
		return nil
	}
	return cronFields(s.sv.cron)
}

func cronFields(spec string) [][2]string {
	fields := strings.Fields(parseCron(spec))
	var out [][2]string
	for i, k := range []string{"Minute", "Hour", "Day", "Month", "Weekday"} {
		v := fields[i]
		if v != "*" {
			v = strconv.Itoa(rubyToI(v))
		}
		out = append(out, [2]string{k, v})
	}
	return out
}

func rubyToI(s string) int {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, _ := strconv.Atoi(s[:end])
	return n
}

var socketRx = regexp.MustCompile(`(?i)^([a-z]+)://(.+):([0-9]+)$`)

func (s renderedService) plist(cmd []string, label string) string {
	d := pdict{"Label": label, "ProgramArguments": cmd, "RunAtLoad": s.get("run_at_load") != rbool(false)}
	if s.get("launch_only_once") == rbool(true) {
		d["LaunchOnlyOnce"] = true
	}
	if s.get("macos_legacy_timers") == rbool(true) {
		d["LegacyTimers"] = true
	}
	for key, field := range map[string]string{"ExitTimeOut": "stop_timeout", "TimeOut": "restart_delay",
		"ThrottleInterval": "throttle_interval", "Nice": "nice"} {
		if v := s.get(field); !blank(v) {
			d[key] = v
		}
	}
	if v := s.get("process_type"); !blank(v) {
		t := str(v)
		d["ProcessType"] = strings.ToUpper(t[:1]) + strings.ToLower(t[1:])
	}
	if v := s.get("interval"); !blank(v) && s.runType() == "interval" {
		d["StartInterval"] = v
	}
	for key, field := range map[string]string{"WorkingDirectory": "working_dir", "RootDirectory": "root_dir",
		"StandardInPath": "input_path", "StandardOutPath": "log_path", "StandardErrorPath": "error_log_path"} {
		if p, ok := s.path(field); ok {
			d[key] = p
		}
	}
	if env := s.env(); len(env) > 0 {
		e := pdict{}
		for _, kv := range env {
			e[kv[0]] = kv[1]
		}
		d["EnvironmentVariables"] = e
	}
	ka := s.keepAlive()
	if always, _ := kwVal(ka, "always"); len(ka.keys) > 0 && always != rbool(false) {
		if always == rbool(true) {
			d["KeepAlive"] = true
		} else if v, ok := kwVal(ka, "successful_exit"); ok {
			d["KeepAlive"] = pdict{"SuccessfulExit": v}
		} else if v, ok := kwVal(ka, "crashed"); ok {
			d["KeepAlive"] = pdict{"Crashed": v}
		} else if v, ok := kwVal(ka, "path"); ok && !blank(v) {
			d["KeepAlive"] = pdict{"PathState": s.str(v)}
		}
	}
	if socks := s.sockets(); len(socks) > 0 {
		d["Sockets"] = socks
	}
	if cron := s.cron(); len(cron) > 0 && s.runType() == "cron" {
		c := pdict{}
		for _, kv := range cron {
			if kv[1] != "*" {
				n, _ := strconv.Atoi(kv[1])
				c[kv[0]] = rint(n)
			}
		}
		d["StartCalendarInterval"] = c
	}
	d["LimitLoadToSessionType"] = []string{"Aqua", "Background", "LoginWindow", "StandardIO", "System"}
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
		"<plist version=\"1.0\">\n")
	emitPlist(&b, d, 0)
	b.WriteString("</plist>\n")
	return b.String()
}

func (s renderedService) sockets() pdict {
	v := s.get("sockets")
	entries := map[string]string{}
	switch x := v.(type) {
	case nil:
		return nil
	case *hash:
		for i, k := range x.keys {
			entries[str(k)] = str(x.vals[i])
		}
	default:
		entries["listeners"] = str(x)
	}
	out := pdict{}
	for name, spec := range entries {
		m := socketRx.FindStringSubmatch(spec)
		if m == nil {
			fail("service sockets %q", spec)
		}
		out[name] = pdict{"SockNodeName": m[2], "SockServiceName": m[3], "SockProtocol": strings.ToUpper(m[1])}
	}
	return out
}

// pdict is a plist dictionary, written with sorted keys.
type pdict map[string]any

// emitPlist writes v as the plist gem does: tab indents, empty elements self-closed.
func emitPlist(b *strings.Builder, v any, level int) {
	ind := strings.Repeat("\t", level)
	tag := func(name, contents string) {
		if contents == "" {
			fmt.Fprintf(b, "%s<%s/>\n", ind, name)
		} else {
			fmt.Fprintf(b, "%s<%s>%s</%s>\n", ind, name, contents, name)
		}
	}
	switch x := v.(type) {
	case pdict:
		if len(x) == 0 {
			tag("dict", "")
			return
		}
		b.WriteString(ind + "<dict>\n")
		for _, k := range slices.Sorted(maps.Keys(x)) {
			fmt.Fprintf(b, "%s\t<key>%s</key>\n", ind, escapeHTML.Replace(k))
			emitPlist(b, x[k], level+1)
		}
		b.WriteString(ind + "</dict>\n")
	case []string:
		if len(x) == 0 {
			tag("array", "")
			return
		}
		b.WriteString(ind + "<array>\n")
		for _, e := range x {
			emitPlist(b, e, level+1)
		}
		b.WriteString(ind + "</array>\n")
	case bool:
		tag(strconv.FormatBool(x), "")
	case rbool:
		tag(strconv.FormatBool(bool(x)), "")
	case string:
		tag("string", escapeHTML.Replace(x))
	case rint:
		tag("integer", strconv.FormatInt(int64(x), 10))
	default:
		tag("string", escapeHTML.Replace(str(x.(value))))
	}
}

var escapeHTML = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")

func quoteSystemd(s string) string {
	r := strings.NewReplacer("\a", `\a`, "\b", `\b`, "\f", `\f`, "\n", `\n`, "\r", `\r`, "\t", `\t`, "\v", `\v`,
		`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

func (s renderedService) unit(cmd []string) string {
	quoted := make([]string, len(cmd))
	for i, c := range cmd {
		quoted[i] = quoteSystemd(c)
	}
	typ := "simple"
	if s.get("launch_only_once") == rbool(true) {
		typ = "oneshot"
	}
	opts := []string{"Type=" + typ, "ExecStart=" + strings.Join(quoted, " ")}
	ka := s.keepAlive()
	if len(ka.keys) > 0 {
		always, _ := kwVal(ka, "always")
		crashed, _ := kwVal(ka, "crashed")
		if always == rbool(true) || crashed == rbool(true) {
			opts = append(opts, "Restart=on-failure")
		} else if v, ok := kwVal(ka, "successful_exit"); ok {
			opts = append(opts, "Restart="+map[bool]string{true: "on-success", false: "on-failure"}[v == rbool(true)])
		}
	}
	for _, o := range [][2]string{{"RestartSec=", "restart_delay"}, {"TimeoutStopSec=", "stop_timeout"}, {"Nice=", "nice"}} {
		if v := s.get(o[1]); !blank(v) {
			opts = append(opts, o[0]+str(v))
		}
	}
	for _, o := range [][2]string{{"WorkingDirectory=", "working_dir"}, {"RootDirectory=", "root_dir"},
		{"StandardInput=file:", "input_path"}, {"StandardOutput=append:", "log_path"}, {"StandardError=append:", "error_log_path"}} {
		if p, ok := s.path(o[1]); ok {
			opts = append(opts, o[0]+p)
		}
	}
	for _, kv := range s.env() {
		opts = append(opts, `Environment="`+kv[0]+"="+kv[1]+`"`)
	}
	return "[Unit]\nDescription=Homebrew generated unit for " + s.sv.f.name + "\n\n[Install]\nWantedBy=default.target\n\n" +
		"[Service]\n" + strings.Join(opts, "\n") + "\n"
}

var weekdays = []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

func (s renderedService) timer(unit string) string {
	var opts []string
	switch s.runType() {
	case "cron":
		opts = append(opts, "Persistent=true")
		c := map[string]string{}
		for _, kv := range s.cron() {
			c[kv[0]] = kv[1]
		}
		two := func(v string) string {
			if v == "*" {
				return v
			}
			n, _ := strconv.Atoi(v)
			return fmt.Sprintf("%02d", n)
		}
		day := ""
		if n, err := strconv.Atoi(c["Weekday"]); err == nil && n >= 0 && n <= 7 {
			day = weekdays[n%7] + " "
		}
		opts = append(opts, "OnCalendar="+day+"*-"+c["Month"]+"-"+c["Day"]+" "+two(c["Hour"])+":"+two(c["Minute"])+":00")
	case "interval":
		opts = append(opts, "OnUnitActiveSec="+str(s.get("interval")))
	}
	return "[Unit]\nDescription=Homebrew generated timer for " + s.sv.f.name + "\n\n[Install]\nWantedBy=timers.target\n\n" +
		"[Timer]\nUnit=" + unit + ".service\n" + strings.Join(opts, "\n") + "\n"
}
