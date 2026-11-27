package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jigsmiths/brw/internal/cask"
)

// caskOptions are brew's cask_options flags, the target directories, and the ones that change how artifacts move.
type caskOptions struct {
	explicit     map[cask.DirKey]string
	force, adopt bool
}

type caskFlags struct {
	dirs map[cask.DirKey]*string
}

func flagName(key cask.DirKey) string { return strings.ReplaceAll(string(key), "_", "-") }

// caskDirFlags registers --appdir, --fontdir and the other target directory flags brew gives its cask commands.
func caskDirFlags(fs *flag.FlagSet) caskFlags {
	f := caskFlags{dirs: map[cask.DirKey]*string{}}
	for _, key := range cask.DirKeys {
		f.dirs[key] = fs.String(flagName(key), "", "target location for "+string(key)+" (default: "+cask.DefaultDir(key, "~")+")")
	}
	return f
}

// options turns the flags that were set into brew's explicit config, with each path expanded as Pathname#expand_path does.
func (f caskFlags) options(fs *flag.FlagSet, home string) caskOptions {
	explicit := map[cask.DirKey]string{}
	fs.Visit(func(set *flag.Flag) {
		for key, value := range f.dirs {
			if set.Name == flagName(key) {
				explicit[key] = cask.ExpandDir(*value, home)
			}
		}
	})
	return caskOptions{explicit: explicit}
}

// rejectWithFormula is brew's `conflicts "--formula", "--fontdir"` for every directory flag that was set.
func (f caskFlags) rejectWithFormula(fs *flag.FlagSet, formulaOnly bool) error {
	if !formulaOnly {
		return nil
	}
	var err error
	fs.Visit(func(set *flag.Flag) {
		for key := range f.dirs {
			if err == nil && set.Name == flagName(key) {
				err = fmt.Errorf("Options --formula and --%s are mutually exclusive.", set.Name) //nolint:staticcheck // matches brew's error text
			}
		}
	})
	return err
}

func caskEnvDirs(home string) map[cask.DirKey]string {
	return cask.DirsFromOptions(strings.Fields(os.Getenv("HOMEBREW_CASK_OPTS")), home)
}

// caskWhere is where artifacts go and what config.json records, for the flags and HOMEBREW_CASK_OPTS in effect.
func caskWhere(prefix, home string, explicit map[cask.DirKey]string) (cask.Where, cask.Config) {
	cfg := cask.Config{Languages: macLanguages(), Explicit: explicit, Env: caskEnvDirs(home)}
	return cask.NewWhere(prefix, "", home).WithConfig(cfg), cfg
}
