package cask

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
)

// DirKey is the config key brew gives a cask target directory, such as "fontdir".
type DirKey string

const (
	DirApp           DirKey = "appdir"
	DirAppImage      DirKey = "appimagedir"
	DirKeyboard      DirKey = "keyboard_layoutdir"
	DirColorPicker   DirKey = "colorpickerdir"
	DirPrefPane      DirKey = "prefpanedir"
	DirQLPlugin      DirKey = "qlplugindir"
	DirMDImporter    DirKey = "mdimporterdir"
	DirDictionary    DirKey = "dictionarydir"
	DirFont          DirKey = "fontdir"
	DirService       DirKey = "servicedir"
	DirInputMethod   DirKey = "input_methoddir"
	DirInternetPlug  DirKey = "internet_plugindir"
	DirAudioUnitPlug DirKey = "audio_unit_plugindir"
	DirVSTPlug       DirKey = "vst_plugindir"
	DirVST3Plug      DirKey = "vst3_plugindir"
	DirScreenSaver   DirKey = "screen_saverdir"
)

// DirKeys lists the directories in the order brew's config writes them.
var DirKeys = []DirKey{
	DirApp, DirAppImage, DirKeyboard, DirColorPicker, DirPrefPane, DirQLPlugin, DirMDImporter, DirDictionary,
	DirFont, DirService, DirInputMethod, DirInternetPlug, DirAudioUnitPlug, DirVSTPlug, DirVST3Plug, DirScreenSaver,
}

const homeMarker = "~"

var defaultDirs = map[DirKey]string{
	DirApp: "/Applications", DirAppImage: "~/Applications", DirKeyboard: "/Library/Keyboard Layouts",
	DirColorPicker: "~/Library/ColorPickers", DirPrefPane: "~/Library/PreferencePanes", DirQLPlugin: "~/Library/QuickLook",
	DirMDImporter: "~/Library/Spotlight", DirDictionary: "~/Library/Dictionaries", DirFont: "~/Library/Fonts",
	DirService: "~/Library/Services", DirInputMethod: "~/Library/Input Methods",
	DirInternetPlug: "~/Library/Internet Plug-Ins", DirAudioUnitPlug: "~/Library/Audio/Plug-Ins/Components",
	DirVSTPlug: "~/Library/Audio/Plug-Ins/VST", DirVST3Plug: "~/Library/Audio/Plug-Ins/VST3",
	DirScreenSaver: "~/Library/Screen Savers",
}

func expandHome(path, home string) string {
	if path == homeMarker || strings.HasPrefix(path, homeMarker+"/") {
		return filepath.Join(home, path[len(homeMarker):])
	}
	return path
}

// DefaultDir is brew's default for key with the home folder filled in.
func DefaultDir(key DirKey, home string) string { return expandHome(defaultDirs[key], home) }

// Dir is where artifacts of key go: the explicit setting, else the environment's, else the default.
func (c Config) Dir(key DirKey, home string) string {
	if d, ok := c.Explicit[key]; ok {
		return d
	}
	if d, ok := c.Env[key]; ok {
		return d
	}
	return DefaultDir(key, home)
}

// Dir is where artifacts of key go.
func (w Where) Dir(key DirKey) string {
	if key == DirApp {
		return w.AppDir
	}
	if d, ok := w.Dirs[key]; ok {
		return d
	}
	return DefaultDir(key, w.Home)
}

// WithConfig sets every directory to what c says.
func (w Where) WithConfig(c Config) Where {
	w.Dirs = map[DirKey]string{}
	for _, key := range DirKeys {
		w.Dirs[key] = c.Dir(key, w.Home)
	}
	w.AppDir = w.Dirs[DirApp]
	return w
}

func orderedDirs(dirs map[DirKey]string) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	for _, key := range DirKeys {
		path, ok := dirs[key]
		if !ok {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		k, _ := json.Marshal(string(key))
		v := jsonString(path)
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// DirsFromOptions reads the --NAMEdir=PATH words of HOMEBREW_CASK_OPTS, as brew does.
func DirsFromOptions(words []string, home string) map[DirKey]string {
	dirs := map[DirKey]string{}
	for _, word := range words {
		flag, value, ok := strings.Cut(word, "=")
		if !ok {
			continue
		}
		key := DirKey(strings.ReplaceAll(strings.TrimPrefix(flag, "--"), "-", "_"))
		if slices.Contains(DirKeys, key) {
			dirs[key] = absolute(expandHome(value, home))
		}
	}
	return dirs
}

// ExpandDir is Pathname#expand_path: a leading ~ is the home folder and a relative path is the working directory's.
func ExpandDir(path, home string) string { return absolute(expandHome(path, home)) }

func absolute(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}
