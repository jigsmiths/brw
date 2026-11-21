package cask

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/platform"
)

type Where struct {
	Prefix string
	AppDir string
	Home   string
}

// Placeholders fills in the API's placeholders as brew does when it loads a cask.
func Placeholders(w Where) func(string) string {
	return strings.NewReplacer(
		"/$HOME", w.Home,
		"$HOMEBREW_PREFIX", w.Prefix,
		"$HOMEBREW_CELLAR", filepath.Join(w.Prefix, "Cellar"),
		"$APPDIR", w.AppDir,
	).Replace
}

// move is an artifact's source, relative to the staged version or absolute,
// and its absolute target.
type move struct {
	source, target string
}

type installPlan struct {
	apps, binaries []move
	generators     []generator
	// preflight runs before the artifacts are installed, postflight after.
	preflight, postflight []json.RawMessage
	// uninstall is the receipt's uninstall_artifacts: the artifacts brew undoes
	// on uninstall, as the API has them with the placeholders filled in.
	uninstall []map[string]json.RawMessage
}

// uninstallDirectives are the uninstall steps brw can carry out.
var uninstallDirectives = []string{"quit", "launchctl", "delete", "trash", "rmdir"}

// plan reads the cask's artifacts. It refuses a cask that needs anything brw
// cannot install or uninstall yet, so that one is never half done.
func plan(c api.Cask, w Where) (installPlan, error) {
	var p installPlan
	unsupported := func(what string) (installPlan, error) {
		return installPlan{}, fmt.Errorf("%s: %s: %w", c.Token, what, ErrUnsupported)
	}
	if len(c.Rename) > 0 {
		return unsupported("rename")
	}
	if len(c.DependsOn.Formula) > 0 || len(c.DependsOn.Cask) > 0 {
		return unsupported("dependencies")
	}
	fill := jsonFiller(Placeholders(w))
	for _, raw := range c.Artifacts {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return installPlan{}, fmt.Errorf("%s: artifact %s: %w", c.Token, raw, err)
		}
		// "target" next to the kind is where the API says the artifact goes, for
		// display; the kind's own arguments say the same.
		delete(obj, "target")
		if len(obj) != 1 {
			return installPlan{}, fmt.Errorf("%s: artifact %s has %d kinds", c.Token, raw, len(obj))
		}
		for kind, args := range obj {
			args = fill(args)
			switch kind {
			case "app", "binary":
				m, err := readMove(args, kind, w)
				if err != nil {
					return installPlan{}, fmt.Errorf("%s: %s %s: %w", c.Token, kind, args, err)
				}
				if kind == "app" {
					p.apps = append(p.apps, m)
				} else {
					p.binaries = append(p.binaries, m)
				}
			case "uninstall":
				if bad := unknownDirectives(args); bad != "" {
					return unsupported("uninstall " + bad)
				}
			case "generate_completions_from_executable":
				g, err := readGenerator(args)
				if err != nil {
					return installPlan{}, fmt.Errorf("%s: %s %s: %w", c.Token, kind, args, err)
				}
				p.generators = append(p.generators, g)
			case "preflight_steps", "postflight_steps":
				steps, err := readSteps(args)
				if err != nil {
					return installPlan{}, fmt.Errorf("%s: %s %s: %w", c.Token, kind, args, err)
				}
				if bad := unsupportedSteps(steps); bad != "" {
					return unsupported(kind + " " + bad)
				}
				if kind == "preflight_steps" {
					p.preflight = append(p.preflight, steps...)
				} else {
					p.postflight = append(p.postflight, steps...)
				}
			case "zap":
			default:
				return unsupported(kind + " artifacts")
			}
			p.uninstall = append(p.uninstall, map[string]json.RawMessage{kind: args})
		}
	}
	if len(p.apps) == 0 && len(p.binaries) == 0 {
		return unsupported("no app or binary")
	}
	return p, nil
}

// jsonFiller fills in placeholders inside JSON string values, keeping the
// order of keys, which brew's receipt keeps from the API.
func jsonFiller(fill func(string) string) func(json.RawMessage) json.RawMessage {
	return func(raw json.RawMessage) json.RawMessage {
		out, err := fillTokens(raw, fill)
		if err != nil {
			return raw
		}
		return out
	}
}

func fillTokens(raw []byte, fill func(string) string) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var b bytes.Buffer
	type frame struct {
		object bool
		n      int // values so far; in an object, keys and values both count
	}
	var stack []frame
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return b.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
		if delim, ok := tok.(json.Delim); ok && (delim == '}' || delim == ']') {
			b.WriteByte(byte(delim))
			stack = stack[:len(stack)-1]
			continue
		}
		isKey := false
		if len(stack) > 0 {
			top := &stack[len(stack)-1]
			switch {
			case top.object && top.n%2 == 1:
				b.WriteByte(':')
			case top.n > 0:
				b.WriteByte(',')
			}
			isKey = top.object && top.n%2 == 0
			top.n++
		}
		switch t := tok.(type) {
		case json.Delim:
			b.WriteByte(byte(t))
			stack = append(stack, frame{object: t == '{'})
		case string:
			if !isKey {
				t = fill(t)
			}
			b.Write(jsonString(t))
		case json.Number:
			b.WriteString(t.String())
		case bool:
			b.WriteString(strconv.FormatBool(t))
		case nil:
			b.WriteString("null")
		}
	}
}

// jsonString encodes s without escaping <, > and &, as Ruby's JSON does.
func jsonString(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// readMove reads an app's or binary's arguments: the source, then options,
// of which brw uses target.
func readMove(args json.RawMessage, kind string, w Where) (move, error) {
	var list []json.RawMessage
	if err := json.Unmarshal(args, &list); err != nil || len(list) == 0 {
		return move{}, fmt.Errorf("unexpected arguments")
	}
	var m move
	if err := json.Unmarshal(list[0], &m.source); err != nil || m.source == "" {
		return move{}, fmt.Errorf("unexpected source")
	}
	target := filepath.Base(m.source)
	for _, opt := range list[1:] {
		var o struct {
			Target string `json:"target"`
		}
		if json.Unmarshal(opt, &o) == nil && o.Target != "" {
			target = o.Target
		}
	}
	dir := w.AppDir
	if kind == "binary" {
		dir = filepath.Join(w.Prefix, "bin")
	}
	switch {
	case target == "~" || strings.HasPrefix(target, "~/"):
		m.target = filepath.Join(w.Home, target[1:])
	case filepath.IsAbs(target):
		m.target = filepath.Clean(target)
	default:
		m.target = filepath.Join(dir, target)
	}
	return m, nil
}

// unknownDirectives names the first uninstall directive brw cannot carry out.
func unknownDirectives(args json.RawMessage) string {
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(args, &list); err != nil {
		return "in an unexpected form"
	}
	for _, d := range list {
		keys := make([]string, 0, len(d))
		for k := range d {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if !slices.Contains(uninstallDirectives, k) {
				return k
			}
		}
	}
	return ""
}

// checkRequirements fails, with brew's message, when the cask cannot run here.
func checkRequirements(c api.Cask, host platform.Host) error {
	fail := func(msg string) error { return fmt.Errorf("%s: %s", c.Token, msg) }
	if host.OS != "darwin" {
		return fail("This cask requires macOS.")
	}
	if m := c.DependsOn.MacOS; m != nil && len(m.Versions) > 0 {
		major, err := platform.MacMajor(host.MacVersion)
		if err != nil {
			return err
		}
		wants := make([]int, len(m.Versions))
		for i, v := range m.Versions {
			if wants[i], err = versionMajor(v); err != nil {
				return fail(fmt.Sprintf("unexpected macOS version %q", v))
			}
		}
		switch m.Comparator {
		case ">=":
			if major < wants[0] {
				return fail("This cask does not run on macOS versions older than " + prettyName(wants[0]) + ".")
			}
		case "<=":
			if major > wants[0] {
				return fail("This cask does not run on macOS versions newer than " + prettyName(wants[0]) + ".")
			}
		default:
			if !slices.Contains(wants, major) {
				names := make([]string, len(wants))
				for i, w := range wants {
					names[i] = prettyName(w)
				}
				msg := names[len(names)-1]
				if len(names) > 1 {
					msg = strings.Join(names[:len(names)-1], ", ") + " and " + msg
				}
				return fail("This cask does not run on macOS versions other than " + msg + ".")
			}
		}
	}
	if len(c.DependsOn.Arch) > 0 {
		here := "arm"
		if host.Arch == "amd64" {
			here = "intel"
		}
		var wanted []string
		ok := false
		for _, a := range c.DependsOn.Arch {
			wanted = append(wanted, fmt.Sprintf("{type: :%s, bits: %d}", a.Type, a.Bits))
			ok = ok || (a.Type == here && a.Bits == 64)
		}
		if !ok {
			return fail(fmt.Sprintf("This cask depends on hardware architecture being one of [%s], but you are running {type: :%s, bits: 64}.",
				strings.Join(wanted, ", "), here))
		}
	}
	return nil
}

// versionMajor reads "12" or "10.15"; any 10.x is older than every supported macOS.
func versionMajor(v string) (int, error) {
	first, _, _ := strings.Cut(v, ".")
	return strconv.Atoi(first)
}

// prettyName is brew's name for a macOS version, such as "Big Sur".
func prettyName(major int) string {
	name, ok := platform.ReleaseName(major)
	if !ok {
		return strconv.Itoa(major)
	}
	words := strings.Split(name, "_")
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}
