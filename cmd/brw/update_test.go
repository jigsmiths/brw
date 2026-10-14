package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/selfupdate"
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

// selfUpdates counts the calls of a fake self-update and returns what it is given.
func selfUpdates(res selfupdate.Result, err error) (func(context.Context) (selfupdate.Result, error), *int) {
	calls := 0
	return func(context.Context) (selfupdate.Result, error) {
		calls++
		return res, err
	}, &calls
}

func TestUpdateUpdatesBrwToo(t *testing.T) {
	hook, calls := selfUpdates(selfupdate.Result{From: "0.1.0", To: "0.2.0", Updated: true}, nil)
	e := &env{host: linux, client: apiServer(t), selfUpdate: hook}
	out, err := runCmd(t, e, "update")
	want := "Updated the formula listing: 19 formulae\nUpdated brw 0.1.0 -> 0.2.0\n"
	if err != nil || out != want || *calls != 1 {
		t.Errorf("out = %q, err = %v, calls = %d", out, err, *calls)
	}
}

func TestUpdateSaysWhenBrwIsUpToDate(t *testing.T) {
	hook, _ := selfUpdates(selfupdate.Result{From: "0.2.0", To: "0.2.0"}, nil)
	e := &env{host: linux, client: apiServer(t), selfUpdate: hook}
	out, err := runCmd(t, e, "update")
	if err != nil || !strings.HasSuffix(out, "brw is up to date (0.2.0)\n") {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

func TestUpdateLeavesLocalBuildsAlone(t *testing.T) {
	hook, _ := selfUpdates(selfupdate.Result{}, selfupdate.ErrNotRelease)
	errOut := &bytes.Buffer{}
	e := &env{host: linux, client: apiServer(t), selfUpdate: hook, err: errOut}
	out, err := runCmd(t, e, "update")
	if err != nil || !strings.HasSuffix(out, "brw was not installed from a release, so it is not updated here\n") || errOut.Len() != 0 {
		t.Errorf("out = %q, err = %v, stderr = %q", out, err, errOut)
	}
}

func TestUpdateWarnsWhenBrwCannotBeUpdated(t *testing.T) {
	hook, _ := selfUpdates(selfupdate.Result{}, errors.New("no network"))
	errOut := &bytes.Buffer{}
	e := &env{host: linux, client: apiServer(t), selfUpdate: hook, err: errOut}
	out, err := runCmd(t, e, "update")
	if err != nil {
		t.Errorf("a failed self-update failed the command: %v", err)
	}
	if out != "Updated the formula listing: 19 formulae\n" {
		t.Errorf("out = %q", out)
	}
	if got := errOut.String(); got != "brw: warning: could not update brw: no network\n" {
		t.Errorf("stderr = %q", got)
	}
}

func TestUpdateCanSkipUpdatingBrw(t *testing.T) {
	hook, calls := selfUpdates(selfupdate.Result{Updated: true}, nil)
	e := &env{host: linux, client: apiServer(t), selfUpdate: hook}
	out, err := runCmd(t, e, "update", "--no-self")
	if err != nil || out != "Updated the formula listing: 19 formulae\n" || *calls != 0 {
		t.Errorf("--no-self: out = %q, err = %v, calls = %d", out, err, *calls)
	}
	t.Setenv("BRW_NO_SELF_UPDATE", "1")
	e.client = apiServer(t)
	if _, err := runCmd(t, e, "update"); err != nil || *calls != 0 {
		t.Errorf("BRW_NO_SELF_UPDATE: err = %v, calls = %d", err, *calls)
	}
}

func TestUpdateDoesNotUpdateBrwWhenTheListingFails(t *testing.T) {
	hook, calls := selfUpdates(selfupdate.Result{Updated: true}, nil)
	e := &env{host: linux, client: &api.Client{URL: "http://127.0.0.1:1", CacheDir: t.TempDir()}, selfUpdate: hook}
	if _, err := runCmd(t, e, "update"); err == nil || *calls != 0 {
		t.Errorf("err = %v, calls = %d", err, *calls)
	}
}
