package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/lock"
	"github.com/jigsmiths/brw/internal/tap"
	"github.com/jigsmiths/brw/internal/trust"
)

const (
	tapsLock = "brw-tap"
	dirPerm  = 0o755
)

var errCoreTap = errors.New("unlike brew, brw reads core from the API, so homebrew/core is not tapped or untapped")

func tapCommand(e *env, args []string) error {
	fs := newFlagSet("tap")
	var dryRun, force, customRemote bool
	prefixArg, journal := changeFlags(e, fs, &dryRun)
	fs.BoolVar(&force, "force", false, "tap homebrew/cask")
	fs.BoolVar(&force, "f", false, "tap homebrew/cask")
	fs.BoolVar(&customRemote, "custom-remote", false, "change the remote of a tapped repository")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	prefix, err := filepath.Abs(*prefixArg)
	if err != nil {
		return err
	}
	switch len(rest) {
	case 0:
		return listTaps(e, prefix)
	case 1, 2:
	default:
		return errors.New("usage: brw tap [--force] [--custom-remote] [USER/REPO [URL]]")
	}
	name, err := tap.ParseName(rest[0])
	if err != nil {
		return err
	}
	if name == tap.CoreName {
		return errCoreTap
	}
	if err := tap.CheckClonable(name); err != nil {
		return err
	}
	explicit := len(rest) == 2
	if customRemote && !explicit {
		return fmt.Errorf("Tap %s with option `--custom-remote` but without a remote URL.", name) //nolint:staticcheck // brew's wording
	}
	requested := tap.DefaultRemote(name)
	if explicit {
		requested = rest[1]
	}
	t := tap.Tap{Name: name, Dir: tap.Dir(prefix, name)}
	_, statErr := os.Stat(t.Dir)
	installed := statErr == nil
	ctx := context.Background()
	if installed && !customRemote {
		if current := t.Remote(); explicit && requested != current {
			return fmt.Errorf("Tap %s remote mismatch.\n%s != %s", name, current, requested) //nolint:staticcheck // brew's wording
		}
		if !tap.Shallow(ctx, t) {
			return nil
		}
	}
	if err := tap.CheckEnv(name, requested, e.err); err != nil {
		return err
	}
	if err := tap.CheckGit(); err != nil {
		return err
	}
	return withChangeRoot(e, prefix, *journal, dryRun, func(root *fsx.Root) error {
		if !dryRun {
			lk, err := lock.Acquire(root, tapsLock)
			if err != nil {
				return err
			}
			defer func() { _ = lk.Release() }()
		}
		if installed {
			return fetchTap(ctx, e, t, requested, dryRun)
		}
		if name == tap.CaskName && !force {
			return errors.New("Tapping homebrew/cask is no longer typically necessary.\nAdd --force if you are sure you need it for contributing to Homebrew.") //nolint:staticcheck // brew's wording
		}
		return cloneTap(ctx, e, root, prefix, t, requested, dryRun)
	})
}

func listTaps(e *env, prefix string) error {
	taps, err := tap.List(prefix)
	if err != nil {
		return err
	}
	names := make([]string, len(taps))
	for i, t := range taps {
		names[i] = t.Name
	}
	slices.Sort(names)
	_, err = fmt.Fprint(e.out, lines(names))
	return err
}

// fetchTap is what brew does for a tapped repository that is shallow or whose remote is changing.
func fetchTap(ctx context.Context, e *env, t tap.Tap, requested string, dryRun bool) error {
	current, shallow := t.Remote(), tap.Shallow(ctx, t)
	if dryRun {
		if requested != current {
			_, _ = fmt.Fprintf(e.out, "would change the remote of %s from %s to %s\n", t.Name, current, requested)
		}
		_, err := fmt.Fprintf(e.out, "would fetch %s%s\n", t.Name, map[bool]string{true: " and unshallow it", false: ""}[shallow])
		return err
	}
	if requested != current {
		if err := tap.FixRemote(ctx, t, requested, e.err); err != nil {
			return err
		}
	}
	if shallow {
		_, _ = fmt.Fprintf(e.err, "==> Unshallowing %s\n", t.Name)
	}
	return tap.Fetch(ctx, t)
}

func cloneTap(ctx context.Context, e *env, root *fsx.Root, prefix string, t tap.Tap, remote string, dryRun bool) error {
	if dryRun {
		_, err := fmt.Fprintf(e.out, "would clone %s into %s\n", remote, t.Dir)
		return err
	}
	_, _ = fmt.Fprintf(e.err, "==> Tapping %s\n", t.Name)
	if err := root.MkdirAll(filepath.Dir(t.Dir), dirPerm); err != nil {
		return err
	}
	if _, err := tap.Clone(ctx, prefix, t.Name, remote); err != nil {
		return errors.Join(err, tap.Remove(root, t))
	}
	if err := tap.LinkManpages(root, prefix, t, e.err); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(e.err, "Tapped%s (%s).\n", contentsOf(t), abv(t))
	store, err := trust.Load(trust.Path())
	if err != nil {
		return err
	}
	state := "is not trusted. Run `brw trust " + t.Name + "` to trust it."
	if store.TapTrusted(t.Name, t.Remote()) {
		state = "is trusted."
	}
	_, err = fmt.Fprintf(e.out, "Tap %s %s\n", t.Name, state)
	return err
}

func contentsOf(t tap.Tap) string {
	c := t.Contents()
	switch len(c) {
	case 0:
		return ""
	case 1:
		return " " + c[0]
	case 2:
		return " " + c[0] + " and " + c[1]
	}
	return " " + strings.Join(c[:len(c)-1], ", ") + ", and " + c[len(c)-1]
}

func abv(t tap.Tap) string {
	s, err := cellar.Keg{Path: t.Dir}.Abv()
	if err != nil {
		return "?"
	}
	return s
}

func withChangeRoot(e *env, prefix, journal string, dryRun bool, fn func(*fsx.Root) error) (err error) {
	if !dryRun {
		return withRoot(prefix, journal, fn)
	}
	root, err := fsx.OpenDryRun(prefix, e.out)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	return fn(root)
}

// ask is brew's Ask.confirm?: it declines unless a person can answer.
func (e *env) ask(action string) bool {
	if e.confirm != nil {
		return e.confirm(action)
	}
	if !e.tty || !isTerminal(os.Stdin) {
		return false
	}
	_, _ = fmt.Fprintf(e.out, "==> Do you want to proceed with the %s? [y/n]\n", action)
	in := bufio.NewReader(os.Stdin)
	for {
		answer, err := in.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y":
			return true
		case "n":
			return false
		}
		if err != nil {
			return false
		}
		_, _ = fmt.Fprintln(e.out, "Invalid input. Please press 'y' to proceed, or 'n' to abort.")
	}
}
