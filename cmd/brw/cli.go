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

// parse reads flags that may come before or after the positional arguments.
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
	return fs.String("prefix", defaultPrefix(e.host), "Homebrew prefix")
}

// changeFlags adds the flags of a command that changes the prefix: --prefix,
// --journal and, when dryRun is not nil, --dry-run and -n.
func changeFlags(e *env, fs *flag.FlagSet, dryRun *bool) (prefix, journal *string) {
	prefix = prefixFlag(e, fs)
	journal = fs.String("journal", "", "append changes to this file")
	if dryRun != nil {
		fs.BoolVar(dryRun, "dry-run", false, "show what would change")
		fs.BoolVar(dryRun, "n", false, "show what would change")
	}
	return prefix, journal
}

// kindFlags adds --formula and --cask, with their plurals. Call the result
// after parsing to read them; it refuses both at once.
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

// lines joins names one per line, with a final newline unless there are none.
func lines(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.Join(names, "\n") + "\n"
}
