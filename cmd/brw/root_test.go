package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRefusesToRunAsRoot(t *testing.T) {
	t.Cleanup(func(old func() bool) func() { return func() { inContainer = old } }(inContainer))
	inContainer = func() bool { return false }
	for cmd, refused := range map[string]bool{"list": true, "install": true, "version": true, "services": false, "bogus": true} {
		e := &env{out: &bytes.Buffer{}, err: &bytes.Buffer{}, euidRoot: true}
		err := run(e, cmd, nil)
		got := err != nil && strings.HasPrefix(err.Error(), "Running brw as root is extremely dangerous and no longer supported.\n")
		if got != refused {
			t.Errorf("%s as root: err = %v, refused = %v, want %v", cmd, err, got, refused)
		}
	}
}

func TestRunAllowsRootInAContainer(t *testing.T) {
	t.Cleanup(func(old func() bool) func() { return func() { inContainer = old } }(inContainer))
	inContainer = func() bool { return true }
	e := &env{out: &bytes.Buffer{}, err: &bytes.Buffer{}, euidRoot: true}
	if err := run(e, "version", nil); err != nil && strings.Contains(err.Error(), "as root") {
		t.Errorf("err = %v", err)
	}
}
