package info

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/platform"
)

// object is a JSON object that keeps its keys in order.
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func parseObject(b []byte) (*object, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok != json.Delim('{') {
		return nil, fmt.Errorf("want a JSON object, got %v", tok)
	}
	o := &object{vals: map[string]json.RawMessage{}}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o.set(key, v)
	}
	return o, nil
}

// set replaces a value in place, or appends a new key.
func (o *object) set(key string, v json.RawMessage) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

func (o *object) setValue(key string, v any) error {
	b, err := marshal(v)
	if err != nil {
		return err
	}
	o.set(key, b)
	return nil
}

func (o *object) delete(key string) {
	delete(o.vals, key)
	o.keys = slices.DeleteFunc(o.keys, func(k string) bool { return k == key })
}

func (o *object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := marshal(k)
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(o.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// expandPlaceholders fills in the prefix the way brew's JSON shows it: paths in
// the service definition are real, while caveats keep $HOMEBREW_PREFIX and
// spell the Cellar as $HOMEBREW_PREFIX/Cellar.
func (o *object) expandPlaceholders(prefix cellar.Prefix) {
	if v, ok := o.vals["service"]; ok {
		escaped, _ := marshal(string(prefix))
		real := string(escaped[1 : len(escaped)-1])
		o.vals["service"] = json.RawMessage(strings.NewReplacer(
			"$HOMEBREW_PREFIX", real, "$HOMEBREW_CELLAR", real+"/Cellar").Replace(string(v)))
	}
	if v, ok := o.vals["caveats"]; ok {
		o.vals["caveats"] = json.RawMessage(strings.ReplaceAll(string(v), "$HOMEBREW_CELLAR", "$HOMEBREW_PREFIX/Cellar"))
	}
}

// marshal encodes v without escaping HTML characters and without a trailing newline.
func marshal(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// JSON returns the entry brew info --json=v2 prints for f, built from the
// formula's raw API entry for this host and the state of the prefix.
func JSON(raw json.RawMessage, f *api.Formula, env Env) (json.RawMessage, error) {
	o, err := parseObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.Name, err)
	}
	tag, err := platform.Tag(env.Host)
	if err != nil {
		return nil, err
	}

	if variations, ok := o.vals["variations"]; ok {
		var byTag map[string]json.RawMessage
		if err := json.Unmarshal(variations, &byTag); err != nil {
			return nil, fmt.Errorf("%s variations: %w", f.Name, err)
		}
		if v, ok := byTag[tag]; ok {
			override, err := parseObject(v)
			if err != nil {
				return nil, fmt.Errorf("%s variation %s: %w", f.Name, tag, err)
			}
			for _, k := range override.keys {
				o.set(k, override.vals[k])
			}
		}
	}
	for _, k := range []string{"variations", "executables", "vulnerabilities"} {
		o.delete(k)
	}

	o.expandPlaceholders(env.Prefix)

	if v, ok := o.vals["versions"]; ok {
		versions, err := parseObject(v)
		if err != nil {
			return nil, fmt.Errorf("%s versions: %w", f.Name, err)
		}
		if _, ok := versions.vals["bottle"]; ok {
			if err := versions.setValue("bottle", env.bottledHere(f)); err != nil {
				return nil, err
			}
			if o.vals["versions"], err = versions.MarshalJSON(); err != nil {
				return nil, err
			}
		}
	}

	bottle, err := hostBottle(f, env.Host, o.vals["bottle"])
	if err != nil {
		return nil, err
	}
	o.set("bottle", bottle)

	kegs := env.installed(f)
	installed := make([]installedJSON, 0, len(kegs))
	var linked any
	for _, k := range kegs {
		installed = append(installed, newInstalledJSON(k))
		if env.Prefix.Linked(f.Name, k.Version) {
			linked = k.Version
		}
	}
	_, pinned := env.Prefix.Pinned(f.Name)
	for key, v := range map[string]any{
		"installed":  installed,
		"linked_keg": linked,
		"pinned":     pinned,
		"outdated":   env.outdatedResult(f.Name) != nil,
	} {
		if err := o.setValue(key, v); err != nil {
			return nil, err
		}
	}
	return o.MarshalJSON()
}

// hostBottle keeps only the bottle file this host would use, or is {} if there is none.
func hostBottle(f *api.Formula, host platform.Host, raw json.RawMessage) (json.RawMessage, error) {
	tag, ok := platform.Select(host, f.BottleTags())
	if !ok || raw == nil {
		return json.RawMessage("{}"), nil
	}
	bottle, err := parseObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%s bottle: %w", f.Name, err)
	}
	stable, err := parseObject(bottle.vals["stable"])
	if err != nil {
		return nil, fmt.Errorf("%s bottle: %w", f.Name, err)
	}
	files, err := parseObject(stable.vals["files"])
	if err != nil {
		return nil, fmt.Errorf("%s bottle files: %w", f.Name, err)
	}
	for _, k := range slices.Clone(files.keys) {
		if k != tag {
			files.delete(k)
		}
	}
	if stable.vals["files"], err = files.MarshalJSON(); err != nil {
		return nil, err
	}
	if bottle.vals["stable"], err = stable.MarshalJSON(); err != nil {
		return nil, err
	}
	return bottle.MarshalJSON()
}

// installedJSON is one entry of "installed": what brew keeps from a receipt.
type installedJSON struct {
	Version             string              `json:"version"`
	UsedOptions         []string            `json:"used_options"`
	BuiltAsBottle       bool                `json:"built_as_bottle"`
	PouredFromBottle    bool                `json:"poured_from_bottle"`
	Time                int64               `json:"time"`
	RuntimeDependencies []cellar.Dependency `json:"runtime_dependencies"`
	InstalledOnRequest  bool                `json:"installed_on_request"`
}

func newInstalledJSON(k keg) installedJSON {
	out := installedJSON{Version: k.Version, UsedOptions: []string{}, RuntimeDependencies: []cellar.Dependency{}}
	if r := k.Receipt; r != nil {
		if r.UsedOptions != nil {
			out.UsedOptions = r.UsedOptions
		}
		if r.RuntimeDependencies != nil {
			out.RuntimeDependencies = r.RuntimeDependencies
		}
		out.BuiltAsBottle, out.PouredFromBottle = r.BuiltAsBottle, r.PouredFromBottle
		out.Time, out.InstalledOnRequest = r.Time, r.InstalledOnRequest
	}
	return out
}
