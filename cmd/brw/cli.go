package main

import (
	"errors"
	"flag"
	"io"
	"strings"
)

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func prefixFlag(e *env, fs *flag.FlagSet) *string {
	return fs.String("prefix", defaultPrefix(e.host), "brw prefix")
}

func changeFlags(e *env, fs *flag.FlagSet, dryRun *bool) (prefix, journal *string) {
	prefix = prefixFlag(e, fs)
	journal = fs.String("journal", "", "append changes to this file")
	if dryRun != nil {
		fs.BoolVar(dryRun, "dry-run", false, "show what would change")
		fs.BoolVar(dryRun, "n", false, "show what would change")
	}
	return prefix, journal
}
func kindFlags(fs *flag.FlagSet) func() (formulaOnly, caskOnly bool, err error) {

	var formula, cask bool
	for _, n := range []string{"formula", "formulae"} {
		fs.BoolVar(&formula, n, false, "only formulae")
	}
	for _, n := range []string{"cask", "casks"} {
		fs.BoolVar(&cask, n, false, "only casks")
	}
	return func() (bool, bool, error) {
		if formula && cask {
			return false, false, errors.New("--formula and --cask are mutually exclusive")
		}
		return formula, cask, nil
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func lines(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.Join(names, "\n") + "\n"
}
