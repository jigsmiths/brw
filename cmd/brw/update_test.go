package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/tap"
)

const listingsUpdated = "Updated the formula listing: 19 formulae\nUpdated the cask listing: 8 casks\n"

func TestUpdate(t *testing.T) {
	e := &env{host: linux, client: apiServer(t)}
	prefix := t.TempDir()
	out, err := runCmd(t, e, "update", "-prefix", prefix)
	if err != nil || out != listingsUpdated {
		t.Errorf("first update = %q, %v", out, err)
	}
	out, err = runCmd(t, e, "update", "-prefix", prefix)
	if err != nil || out != "Already up-to-date.\n" {
		t.Errorf("second update = %q, %v", out, err)
	}
}

func TestUpdateFailsOffline(t *testing.T) {
	c := apiServer(t)
	e := &env{host: linux, client: c}
	prefix := t.TempDir()
	if _, err := runCmd(t, e, "update", "-prefix", prefix); err != nil {
		t.Fatal(err)
	}
	e.client = &api.Client{URL: "http://127.0.0.1:1", CacheDir: c.CacheDir}
	out, err := runCmd(t, e, "update", "-prefix", prefix)
	if err == nil || out != "" {
		t.Errorf("update offline = %q, %v; want an error and no output", out, err)
	}
}

func TestUpdateFailsWhenTheCaskListingFails(t *testing.T) {
	c := apiServer(t)
	c.CaskURL = "http://127.0.0.1:1"
	out, err := runCmd(t, &env{host: linux, client: c}, "update", "-prefix", t.TempDir())
	if err == nil || out != "" {
		t.Errorf("update = %q, %v; want an error and no output", out, err)
	}
}

func TestUpdateLeavesHomebrewCoreAlone(t *testing.T) {
	e := &env{host: linux, client: apiServer(t), err: &bytes.Buffer{}}
	prefix := t.TempDir()
	if err := os.MkdirAll(filepath.Join(prefix, "Library/Taps/homebrew/homebrew-core"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, e, "update", "-prefix", prefix); err != nil {
		t.Fatal(err)
	}
	if w := e.err.(*bytes.Buffer).String(); w != "" {
		t.Errorf("stderr = %q", w)
	}
}

func TestUpdatePullsTapsTogetherAndReportsThemInOrder(t *testing.T) {
	e := &env{host: linux, client: apiServer(t), err: &bytes.Buffer{}}
	prefix := t.TempDir()
	names := []string{"a", "b", "c"}
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(prefix, "Library/Taps/u/homebrew-"+n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var started sync.WaitGroup
	started.Add(len(names))
	e.pull = func(ctx context.Context, tp tap.Tap) (bool, error) {
		started.Done()
		all := make(chan struct{})
		go func() { started.Wait(); close(all) }()
		select {
		case <-all:
		case <-time.After(5 * time.Second):
			return false, errors.New("pulled alone")
		}
		switch tp.Name {
		case "u/a":
			time.Sleep(50 * time.Millisecond)
			return false, errors.New("a offline")
		case "u/b":
			return false, errors.New("b offline")
		}
		return true, nil
	}
	out, err := runCmd(t, e, "update", "-prefix", prefix)
	if err != nil || out != listingsUpdated+"Updated 1 tap (u/c).\n" {
		t.Errorf("update = %q, %v", out, err)
	}
	want := "brw: warning: could not update u/a: a offline\nbrw: warning: could not update u/b: b offline\n"
	if w := e.err.(*bytes.Buffer).String(); w != want {
		t.Errorf("stderr = %q, want %q", w, want)
	}
}

func TestUpdateTakesNoArguments(t *testing.T) {
	e := &env{host: linux, client: apiServer(t)}
	if _, err := runCmd(t, e, "update", "-prefix", t.TempDir(), "jq"); err == nil || !strings.Contains(err.Error(), "usage: brw update") {
		t.Errorf("error = %v", err)
	}
}

func TestUpdateSummary(t *testing.T) {
	unchanged := api.Update{Total: 7}
	for taps, want := range map[string]string{
		"jigsmiths/tap": "Updated 1 tap (jigsmiths/tap).\n",
		"a/b c/d e/f":   "Updated 3 taps (a/b, c/d and e/f).\n",
	} {
		if got := updateSummary(unchanged, unchanged, strings.Fields(taps)); got != want {
			t.Errorf("taps %s: got %q, want %q", taps, got, want)
		}
	}
	for _, tc := range []struct {
		formulae, casks api.Update
		want            string
	}{
		{api.Update{}, api.Update{}, "Already up-to-date.\n"},
		{api.Update{Changed: true, Total: 5}, unchanged, "Updated the formula listing: 5 formulae\n"},
		{api.Update{Changed: true, Total: 5, Added: []string{"a", "b"}}, unchanged, "Updated the formula listing: 5 formulae (+2 new)\n"},
		{api.Update{Changed: true, Total: 5, Removed: []string{"a"}}, unchanged, "Updated the formula listing: 5 formulae (-1 removed)\n"},
		{api.Update{Changed: true, Total: 5, Added: []string{"a"}, Removed: []string{"b", "c", "d"}}, unchanged, "Updated the formula listing: 5 formulae (+1 new, -3 removed)\n"},
		{unchanged, api.Update{Changed: true, Total: 9, Added: []string{"x"}}, "Updated the cask listing: 9 casks (+1 new)\n"},
		{api.Update{Changed: true, Total: 5}, api.Update{Changed: true, Total: 9},
			"Updated the formula listing: 5 formulae\nUpdated the cask listing: 9 casks\n"},
	} {
		if got := updateSummary(tc.formulae, tc.casks, nil); got != tc.want {
			t.Errorf("%+v, %+v: got %q, want %q", tc.formulae, tc.casks, got, tc.want)
		}
	}
}
