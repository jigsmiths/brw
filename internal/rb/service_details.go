package rb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

type ServiceDetails struct {
	Defined      bool
	Command      []string
	Env          [][2]string
	Fields       map[string]string
	Cron         [][2]string
	KeepAlive    bool
	Timed        bool
	RequiresRoot bool
	NamesMac     bool
	MacName      string
}

var detailFields = []string{"working_dir", "root_dir", "input_path", "log_path", "error_log_path", "interval"}

var cronOrder = []string{"Month", "Day", "Weekday", "Hour", "Minute"}

func (f *Formula) ServiceDetails(root string) (d ServiceDetails, err error) {
	if f.b.serviceBlk == nil {
		return ServiceDetails{}, nil
	}
	defer catch(f.b.name, &err)
	s := f.renderService(root)
	d = ServiceDetails{Defined: true, Command: s.command(), Timed: s.timed(), Fields: map[string]string{},
		RequiresRoot: s.get("require_root") == rbool(true)}
	ka := s.keepAlive()
	always, _ := kwVal(ka, "always")
	d.KeepAlive = len(ka.keys) > 0 && always != rbool(false)
	d.Env = s.env()
	for _, k := range detailFields {
		if v := s.get(k); v != nil {
			d.Fields[k] = s.str(v)
		}
	}
	d.Cron = brewCronOrder(s.cron())
	if h, ok := s.get("name").(*hash); ok {
		if v, ok := h.get(rstr("macos")); ok && v != nil {
			d.NamesMac, d.MacName = true, str(v)
		}
	}
	return d, nil
}

func brewCronOrder(fields [][2]string) [][2]string {
	if len(fields) == 0 {
		return nil
	}
	byName := map[string]string{}
	for _, kv := range fields {
		byName[kv[0]] = kv[1]
	}
	var out [][2]string
	for _, k := range cronOrder {
		out = append(out, [2]string{k, byName[k]})
	}
	return out
}

func ServiceFromAPI(raw []byte, root string) (d ServiceDetails, err error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ServiceDetails{}, nil
	}
	var s struct {
		Run          json.RawMessage   `json:"run"`
		RunType      string            `json:"run_type"`
		Cron         string            `json:"cron"`
		KeepAlive    map[string]any    `json:"keep_alive"`
		RequireRoot  bool              `json:"require_root"`
		Env          json.RawMessage   `json:"environment_variables"`
		Name         map[string]string `json:"name"`
		WorkingDir   *string           `json:"working_dir"`
		RootDir      *string           `json:"root_dir"`
		InputPath    *string           `json:"input_path"`
		LogPath      *string           `json:"log_path"`
		ErrorLogPath *string           `json:"error_log_path"`
		Interval     *int              `json:"interval"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return ServiceDetails{}, err
	}
	sub := func(v string) string { return strings.ReplaceAll(v, "$HOMEBREW_PREFIX", root) }
	d = ServiceDetails{Defined: true, Fields: map[string]string{}, RequiresRoot: s.RequireRoot,
		Timed: s.RunType == "cron" || s.RunType == "interval"}
	d.MacName, d.NamesMac = s.Name["macos"]
	always, hasAlways := s.KeepAlive["always"]
	d.KeepAlive = len(s.KeepAlive) > 0 && (!hasAlways || always != false)
	if d.Command, err = apiCommand(s.Run, sub); err != nil {
		return ServiceDetails{}, err
	}
	for k, v := range map[string]*string{"working_dir": s.WorkingDir, "root_dir": s.RootDir, "input_path": s.InputPath,
		"log_path": s.LogPath, "error_log_path": s.ErrorLogPath} {
		if v != nil {
			d.Fields[k] = sub(*v)
		}
	}
	if s.Interval != nil {
		d.Fields["interval"] = fmt.Sprint(*s.Interval)
	}
	if s.Cron != "" {
		d.Cron = brewCronOrder(cronFields(s.Cron))
	}
	d.Env, err = orderedStrings(s.Env, sub)
	return d, err
}

func apiCommand(run json.RawMessage, sub func(string) string) ([]string, error) {
	if len(run) == 0 || string(run) == "null" {
		return nil, nil
	}
	var perOS map[string]json.RawMessage
	if json.Unmarshal(run, &perOS) == nil {
		run = perOS["macos"]
	}
	var list []string
	if json.Unmarshal(run, &list) != nil {
		var one string
		if err := json.Unmarshal(run, &one); err != nil {
			return nil, errors.New("unreadable service run")
		}
		list = []string{one}
	}
	out := make([]string, len(list))
	for i, a := range list {
		out[i] = sub(a)
		if strings.HasPrefix(out[i], "~") {
			p, err := ExpandPath(out[i])
			if err != nil {
				return nil, err
			}
			out[i] = p
		}
	}
	return out, nil
}

func orderedStrings(raw json.RawMessage, sub func(string) string) ([][2]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var out [][2]string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v string
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, [2]string{k.(string), sub(v)})
	}
	return slices.Clip(out), nil
}
