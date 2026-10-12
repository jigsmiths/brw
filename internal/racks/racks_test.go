package racks

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/jigsmiths/brw/internal/cellar"
)

func finder(resolve func(string) (string, bool)) Finder {
	return Finder{
		Prefix:  "/p",
		Kegs:    []cellar.Keg{{Name: "jq", Version: "1.0"}, {Name: "jq", Version: "2.0"}, {Name: "tree", Version: "1"}},
		Resolve: resolve,
	}
}

func TestFindInstalledNamesAliasesAndTapPrefix(t *testing.T) {
	f := finder(func(n string) (string, bool) {
		if n == "jqalias" {
			return "jq", true
		}
		return n, false
	})
	for in, want := range map[string]string{"jq": "jq", "JQ": "jq", "homebrew/core/tree": "tree", "jqalias": "jq"} {
		got, err := f.Find(in)
		if err != nil || got != want {
			t.Errorf("Find(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if got := f.Installed("jq"); len(got) != 2 {
		t.Errorf("Installed(jq) = %v", got)
	}
}

func TestFindSaysWhyANameIsMissing(t *testing.T) {
	known := map[string]bool{"ack": true}
	f := finder(func(n string) (string, bool) { return n, known[n] })
	_, err := f.Find("ack")
	var nk *NoSuchKegError
	if !errors.As(err, &nk) || nk.Path != filepath.Join("/p", "Cellar", "ack") || err.Error() != "no such keg: /p/Cellar/ack" {
		t.Errorf("a known formula that is not installed: %v", err)
	}
	_, err = f.Find("nosuchthing")
	var un *UnavailableError
	if !errors.As(err, &un) || err.Error() != `no available formula with the name "nosuchthing"` {
		t.Errorf("an unknown name: %v", err)
	}
	// Without a listing, the missing keg is reported.
	f.Resolve = nil
	if _, err := f.Find("nosuchthing"); !errors.As(err, &nk) {
		t.Errorf("no listing: %v", err)
	}
}

func TestFindWhenAnAliasPointsAtSomethingNotInstalled(t *testing.T) {
	f := finder(func(n string) (string, bool) { return "ack", n == "ackalias" })
	_, err := f.Find("ackalias")
	var nk *NoSuchKegError
	if !errors.As(err, &nk) || nk.Path != "/p/Cellar/ack" {
		t.Errorf("err = %v", err)
	}
}
