package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jigsmiths/brw/internal/tap"
	"github.com/jigsmiths/brw/internal/trust"
)

// trustListing are the store's kinds as brew trust lists them.
var trustListing = []struct{ key, label string }{
	{"trustedtaps", "taps"}, {"trustedformulae", "formulae"}, {"trustedcasks", "casks"}, {"trustedcommands", "commands"},
}

// trustCommand trusts taps and casks in brew's trust store, as brew trust
// does, or lists what is trusted.
func trustCommand(e *env, args []string) error {
	return trustOrUntrust(e, "trust", args)
}

// untrustCommand removes taps and casks from brew's trust store. Untrusting a
// tap also removes its casks, as brew untrust does.
func untrustCommand(e *env, args []string) error {
	return trustOrUntrust(e, "untrust", args)
}

func trustOrUntrust(e *env, cmd string, args []string) error {
	fs := newFlagSet(cmd)
	prefix := prefixFlag(e, fs)
	asTap := fs.Bool("tap", false, "the name is a tap")
	asCask := fs.Bool("cask", false, "the name is a cask")
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *asTap && *asCask {
		return errors.New("--tap and --cask cannot be used together")
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
		kind, ref, err := trustTarget(*prefix, name, *asTap, *asCask)
		if err != nil {
			return err
		}
		label := "cask"
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
			for _, c := range store.Entries(trust.KindCask) {
				if strings.HasPrefix(c, ref+"/") {
					removed, err := store.Untrust(trust.KindCask, c)
					if err != nil {
						return err
					}
					changed = changed || removed
				}
			}
		}
		fmt.Fprintf(&b, "%s %s: %s\n", map[bool]string{true: "Untrusted", false: "Not trusted"}[changed], label, ref)
	}
	_, err = fmt.Fprint(e.out, b.String())
	return err
}

// trustTarget is how the store names a tap (user/repo) or a cask
// (user/repo/token): by the tap's remote when it was tapped from elsewhere
// than GitHub.
func trustTarget(prefix, name string, asTap, asCask bool) (trust.Kind, string, error) {
	slashes := strings.Count(name, "/")
	switch {
	case !asCask && (asTap || slashes == 1) && slashes == 1:
		t := tap.Tap{Name: strings.ToLower(name), Dir: tap.Dir(prefix, strings.ToLower(name))}
		return trust.KindTap, trust.Reference(t.Name, t.Remote()), nil
	case !asTap && slashes == 2:
		tapName, token, ok := tap.Split(name)
		if !ok {
			break
		}
		t := tap.Tap{Name: tapName, Dir: tap.Dir(prefix, tapName)}
		return trust.KindCask, trust.Reference(t.Name, t.Remote()) + "/" + strings.ToLower(token), nil
	}
	return "", "", fmt.Errorf("trust targets must be fully-qualified tap or cask names: %s", name)
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
