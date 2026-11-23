package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
)

// listingsUpdated is what a first update of the test listings prints.
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

// brw reads homebrew/core from the API, so pulling its tap is wasted time.
func TestUpdateLeavesHomebrewCoreAlone(t *testing.T) {
	e := &env{host: linux, client: apiServer(t), err: &bytes.Buffer{}}
	prefix := t.TempDir()
	// Not a clone, so a pull would fail and warn.
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
