package leaves

import (
	"reflect"
	"testing"

	"github.com/jigsmiths/brw/internal/cellar"
)

func keg(name, version, tap string, onRequest bool, deps ...string) cellar.Keg {
	r := &cellar.Receipt{InstalledOnRequest: onRequest, Source: cellar.Source{Tap: tap}}
	for _, d := range deps {
		r.RuntimeDependencies = append(r.RuntimeDependencies, cellar.Dependency{FullName: d})
	}
	return cellar.Keg{Name: name, Version: version, Receipt: r}
}

func TestList(t *testing.T) {
	kegs := []cellar.Keg{
		keg("git", "2.0", "homebrew/core", true, "pcre2", "homebrew/core/gettext"),
		keg("pcre2", "10.1", "homebrew/core", false),
		keg("gettext", "0.2", "homebrew/core", false),
		keg("jq", "1.8", "homebrew/core", true, "oniguruma"),
		keg("oniguruma", "6.9", "homebrew/core", false),
		keg("libpq", "18", "homebrew/core", false), // needed by a cask only
		keg("orphan", "1", "homebrew/core", false), // a leaf nobody asked for
		keg("tool", "3", "someone/tap", true),
		keg("tool", "2", "someone/tap", true), // an older keg of the same rack
	}
	casks := map[string][]string{"libpq": {"pgadmin4"}}

	tests := []struct {
		name   string
		filter Filter
		want   []string
	}{
		{"all", All, []string{"git", "jq", "orphan", "someone/tap/tool"}},
		{"on request", OnRequest, []string{"git", "jq", "someone/tap/tool"}},
		{"as dependency", AsDependency, []string{"orphan"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := List(cellar.Prefix(t.TempDir()), kegs, casks, tt.filter); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("List = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestListNeedsNoReceipts(t *testing.T) {
	kegs := []cellar.Keg{{Name: "b", Version: "1"}, {Name: "a", Version: "1"}}
	if got, want := List(cellar.Prefix(t.TempDir()), kegs, nil, All), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("List = %v, want %v", got, want)
	}
	if got := List(cellar.Prefix(t.TempDir()), kegs, nil, OnRequest); len(got) != 0 {
		t.Errorf("without receipts nothing was installed on request, got %v", got)
	}
}
