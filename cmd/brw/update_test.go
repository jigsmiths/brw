package main

import (
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
)

func TestUpdate(t *testing.T) {
	e := &env{host: linux, client: apiServer(t)}
	out, err := runCmd(t, e, "update")
	if err != nil || out != "Updated the formula listing: 19 formulae\n" {
		t.Errorf("first update = %q, %v", out, err)
	}
	out, err = runCmd(t, e, "update")
	if err != nil || out != "Already up-to-date.\n" {
		t.Errorf("second update = %q, %v", out, err)
	}
}

func TestUpdateFailsOffline(t *testing.T) {
	c := apiServer(t)
	e := &env{host: linux, client: c}
	if _, err := runCmd(t, e, "update"); err != nil {
		t.Fatal(err)
	}
	e.client = &api.Client{URL: "http://127.0.0.1:1", CacheDir: c.CacheDir}
	out, err := runCmd(t, e, "update")
	if err == nil || out != "" {
		t.Errorf("update offline = %q, %v; want an error and no output", out, err)
	}
}

func TestUpdateTakesNoArguments(t *testing.T) {
	e := &env{host: linux, client: apiServer(t)}
	if _, err := runCmd(t, e, "update", "jq"); err == nil || !strings.Contains(err.Error(), "usage: brw update") {
		t.Errorf("error = %v", err)
	}
}

func TestUpdateSummary(t *testing.T) {
	for _, tc := range []struct {
		up   api.Update
		want string
	}{
		{api.Update{}, "Already up-to-date.\n"},
		{api.Update{Changed: true, Total: 5}, "Updated the formula listing: 5 formulae\n"},
		{api.Update{Changed: true, Total: 5, Added: []string{"a", "b"}}, "Updated the formula listing: 5 formulae (+2 new)\n"},
		{api.Update{Changed: true, Total: 5, Removed: []string{"a"}}, "Updated the formula listing: 5 formulae (-1 removed)\n"},
		{api.Update{Changed: true, Total: 5, Added: []string{"a"}, Removed: []string{"b", "c", "d"}}, "Updated the formula listing: 5 formulae (+1 new, -3 removed)\n"},
	} {
		if got := updateSummary(tc.up); got != tc.want {
			t.Errorf("%+v: got %q, want %q", tc.up, got, tc.want)
		}
	}
}
