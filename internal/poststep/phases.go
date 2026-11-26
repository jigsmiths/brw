package poststep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type phase string

const (
	phaseInstall   phase = "install"
	phaseUninstall phase = "uninstall"
)

// requiresSudo is brew's sudo_required? for one step; the optional kinds count when routing steps out of the sandbox.
func (s step) requiresSudo(includeOptional bool) bool {
	return s.Sudo == sudoAlways || s.Type == "delete_keychain_certificate" ||
		(includeOptional && (s.Sudo == sudoIfNeeded || s.Type == "set_ownership"))
}

// Run runs nothing if any step is unsupported.
func Run(ctx context.Context, c Context, raw []json.RawMessage) error {
	if u := Unsupported(raw); len(u) > 0 {
		return fmt.Errorf("unsupported post_install steps: %s", strings.Join(u, ", "))
	}
	return execute(ctx, c, raw, phaseInstall)
}

// Uninstall removes only the symlinks of steps marked uninstall, and only if they
// still lead where the step pointed them.
func Uninstall(ctx context.Context, c Context, raw []json.RawMessage) error {
	return execute(ctx, c, raw, phaseUninstall)
}

func execute(ctx context.Context, c Context, raw []json.RawMessage, ph phase) error {
	if c.Sandbox != Sandboxed && c.Sandbox != Unsandboxed {
		return errors.New("the context does not say whether steps are sandboxed")
	}
	r := &runner{ctx: ctx, c: c, guards: map[string]bool{}}
	if c.Sandbox == Sandboxed && c.OS == "darwin" {
		ran, err := r.runInSandbox(raw, ph)
		if ran || err != nil {
			return err
		}
	}
	return r.plan(raw, ph, nil)
}

// plan runs the steps in order; steps that need sudo go to privileged when there is one.
func (r *runner) plan(raw []json.RawMessage, ph phase, privileged func(index int) error) error {
	for i, b := range raw {
		s, err := r.parse(ph, i, b)
		if err != nil {
			return err
		}
		if ph == phaseInstall && !r.guardsMatch(s) {
			continue
		}
		if privileged != nil && s.requiresSudo(true) {
			if err := privileged(i); err != nil {
				return err
			}
			continue
		}
		if err := r.apply(ph, i, s); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) parse(ph phase, i int, b json.RawMessage) (step, error) {
	var s step
	if err := json.Unmarshal(b, &s); err != nil {
		if ph == phaseInstall {
			return s, fmt.Errorf("post_install step %d: %w", i+1, err)
		}
		return s, fmt.Errorf("step %d: %w", i+1, err)
	}
	return s, nil
}

func (r *runner) apply(ph phase, i int, s step) error {
	if ph == phaseUninstall {
		if s.Type != "symlink" || !s.Uninstall || s.Source == nil {
			return nil
		}
		if err := r.uninstallLink(s); err != nil {
			return fmt.Errorf("step %d: %w", i+1, err)
		}
		return nil
	}
	if err := r.do(s); err != nil {
		return fmt.Errorf("post_install step %d (%s): %w", i+1, s.Type, err)
	}
	return nil
}

// stepAt is one step of a plan the parent already vetted, run on the parent's behalf.
func (r *runner) stepAt(ph phase, raw []json.RawMessage, index int) error {
	if index < 0 || index >= len(raw) {
		return fmt.Errorf("Invalid privileged cask install step index: %d", index) //nolint:staticcheck // brew's message, shown to the user
	}
	s, err := r.parse(ph, index, raw[index])
	if err != nil {
		return err
	}
	if !s.requiresSudo(true) {
		return fmt.Errorf("Cask install step %d is not privileged.", index) //nolint:staticcheck // brew's message, shown to the user
	}
	if ph == phaseInstall && !r.guardsMatch(s) {
		return nil
	}
	return r.apply(ph, index, s)
}
