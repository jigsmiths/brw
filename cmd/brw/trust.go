package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jigsmiths/brw/internal/tap"
	"github.com/jigsmiths/brw/internal/trust"
)

var trustListing = []struct{ key, label string }{
	{"trustedtaps", "taps"}, {"trustedformulae", "formulae"}, {"trustedcasks", "casks"}, {"trustedcommands", "commands"},
}

func trustCommand(e *env, args []string) error {
	return trustOrUntrust(e, "trust", args)
}

func untrustCommand(e *env, args []string) error {
	return trustOrUntrust(e, "untrust", args)
}

func trustOrUntrust(e *env, cmd string, args []string) error {
	fs := newFlagSet(cmd)
	prefix := prefixFlag(e, fs)
	asTap := fs.Bool("tap", false, "the name is a tap")
	asFormula := fs.Bool("formula", false, "the name is a formula")
	asCask := fs.Bool("cask", false, "the name is a cask")
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	if countTrue(*asTap, *asFormula, *asCask) > 1 {
		return errors.New("--tap, --formula and --cask cannot be used together")
	}
	store, err := trust.Load(trust.Path())
	if err != nil {
		return err
	}
	if len(names) == 0 {
		if cmd == "untrust" {
			return errors.New("usage: brw untrust [--tap|--cask] NAME [NAME ...]")
		}
		return listTrusted(e, store)
	}
	var b strings.Builder
	for _, name := range names {
		kind, ref, err := trustTarget(*prefix, name, *asTap, *asFormula, *asCask)
		if err != nil {
			return err
		}
		label := map[trust.Kind]string{trust.KindFormula: "formula", trust.KindCask: "cask"}[kind]
		if kind == trust.KindTap {
			label = "tap"
			if strings.HasPrefix(ref, "homebrew/") {
				fmt.Fprintf(&b, "Official tap %s is always trusted.\n", ref)
				continue
			}
		}
		var changed bool
		if cmd == "trust" {
			if changed, err = store.Trust(kind, ref); err != nil {
				return err
			}
			fmt.Fprintf(&b, "%s %s: %s\n", map[bool]string{true: "Trusted", false: "Already trusted"}[changed], label, ref)
			continue
		}
		if changed, err = store.Untrust(kind, ref); err != nil {
			return err
		}
		if kind == trust.KindTap {
			for _, k := range []trust.Kind{trust.KindFormula, trust.KindCask} {
				for _, c := range store.Entries(k) {
					if strings.HasPrefix(c, ref+"/") {
						removed, err := store.Untrust(k, c)
						if err != nil {
							return err
						}
						changed = changed || removed
					}
				}
			}
		}
		fmt.Fprintf(&b, "%s %s: %s\n", map[bool]string{true: "Untrusted", false: "Not trusted"}[changed], label, ref)
	}
	_, err = fmt.Fprint(e.out, b.String())
	return err
}

// The store names a tap user/repo and a formula or cask user/repo/name, by the tap's remote when not from GitHub.
func trustTarget(prefix, name string, asTap, asFormula, asCask bool) (trust.Kind, string, error) {
	slashes := strings.Count(name, "/")
	switch {
	case !asFormula && !asCask && slashes == 1:
		t := tap.Tap{Name: strings.ToLower(name), Dir: tap.Dir(prefix, strings.ToLower(name))}
		return trust.KindTap, trust.Reference(t.Name, t.Remote()), nil
	case !asTap && slashes == 2:
		tapName, token, ok := tap.Split(name)
		if !ok {
			break
		}
		t := tap.Tap{Name: tapName, Dir: tap.Dir(prefix, tapName)}
		kind := trust.KindCask
		switch {
		case asFormula:
			kind = trust.KindFormula
		case asCask:
		case t.HasFormula(token) && t.HasCask(token):
			return "", "", fmt.Errorf("ambiguous trust target %s: use --formula or --cask", name)
		case t.HasFormula(token):
			kind = trust.KindFormula
		}
		return kind, trust.Reference(t.Name, t.Remote()) + "/" + strings.ToLower(token), nil
	}
	return "", "", fmt.Errorf("trust targets must be fully-qualified tap, formula or cask names: %s", name)
}

func countTrue(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

func listTrusted(e *env, store *trust.Store) error {
	var b strings.Builder
	b.WriteString("All official taps and commands are trusted.\n")
	printed := false
	for _, l := range trustListing {
		entries := store.Entries(trust.Kind(l.key))
		if len(entries) == 0 {
			continue
		}
		fmt.Fprintf(&b, "Trusted %s:\n", l.label)
		for _, v := range entries {
			fmt.Fprintf(&b, "  %s\n", v)
		}
		printed = true
	}
	if !printed {
		b.WriteString("No trusted taps, formulae, casks or commands.\n")
	}
	_, err := fmt.Fprint(e.out, b.String())
	return err
}
