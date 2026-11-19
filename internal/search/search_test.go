package search

import (
	"os"
	"reflect"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
)

func index(t *testing.T) *api.Index {
	t.Helper()
	b, err := os.ReadFile("../api/testdata/formulae.json")
	if err != nil {
		t.Fatal(err)
	}
	fs, err := api.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return api.NewIndex(fs)
}

func caskIndex(t *testing.T, extra ...api.Cask) *api.CaskIndex {
	t.Helper()
	b, err := os.ReadFile("../api/testdata/casks.json")
	if err != nil {
		t.Fatal(err)
	}
	cs, err := api.ParseCasks(b)
	if err != nil {
		t.Fatal(err)
	}
	return api.NewCaskIndex(append(cs, extra...))
}

func TestCaskTokens(t *testing.T) {
	idx := caskIndex(t)
	for query, want := range map[string][]string{
		"1password":            {"1password-cli", "1password@nightly"},
		"CLI":                  {"1password-cli", "gcloud-cli"},
		"4k-video-downloader+": {"4k-video-downloader+"},
		"/^a/":                 {"adobe-connect", "aptible"},
		"zzzqqq":               nil,
	} {
		got, err := CaskTokens(idx, query)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("CaskTokens(%q) = %q, %v; want %q", query, got, err, want)
		}
	}
	if _, err := CaskTokens(idx, "/(/"); err == nil {
		t.Error("an invalid regex should fail")
	}
}

func TestCaskDescriptions(t *testing.T) {
	idx := caskIndex(t, api.Cask{Token: "nodesc", Names: []string{"Nightly Thing"}})
	for query, want := range map[string][]string{
		"google":      {"gcloud-cli"},        // the description
		"nightly":     {"1password@nightly"}, // the name; nodesc has no description
		"/^Password/": {"1password@nightly"},
		"zzzqqq":      nil,
	} {
		got, err := CaskDescriptions(idx, query)
		if err != nil {
			t.Fatal(err)
		}
		var tokens []string
		for _, m := range got {
			tokens = append(tokens, m.Token)
		}
		if !reflect.DeepEqual(tokens, want) {
			t.Errorf("CaskDescriptions(%q) = %q, want %q", query, tokens, want)
		}
	}
	got, _ := CaskDescriptions(idx, "google")
	if got[0] != (CaskMatch{"gcloud-cli", "Google Cloud CLI", "Set of tools to manage resources and applications hosted on Google Cloud"}) {
		t.Errorf("match = %+v", got[0])
	}
}

func TestNames(t *testing.T) {
	tests := []struct {
		query string
		want  []string
	}{
		{"jq", []string{"jq"}},
		{"JQ", []string{"jq"}},
		{"json", []string{"json-c"}},
		{"python@3", []string{"python@3.13"}},
		{"python 3", nil},                         // spaces drop out but "@" stays, so "python3" is not in "python@313"
		{"sqlite", []string{"sqlite"}},            // alias sqlite3 dropped, its formula matches
		{"nodejs", []string{"node.js", "nodejs"}}, // aliases match when the formula does not
		{"sqlite3", []string{"sqlite3"}},
		{"c++", nil},
		{"/^lib/", []string{"libmpdec", "libunistring"}},
		{"/^JQ/", nil}, // regexes are case sensitive
		{"nothing-like-this", nil},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			got, err := Names(index(t), tt.query)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Names(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestNamesInvalidRegex(t *testing.T) {
	if _, err := Names(index(t), "/(/"); err == nil {
		t.Error("expected error")
	}
}

func TestDescriptions(t *testing.T) {
	got, err := Descriptions(index(t), "json")
	if err != nil {
		t.Fatal(err)
	}
	want := []Match{
		{"jq", "Lightweight and flexible command-line JSON processor"},
		{"json-c", "JSON parser for C"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	if got, _ = Descriptions(index(t), "/^lightweight/"); len(got) != 0 {
		t.Errorf("regex is case sensitive, got %v", got)
	}
	if got, _ = Descriptions(index(t), "/^Lightweight/"); len(got) != 1 || got[0].Name != "jq" {
		t.Errorf("got %v", got)
	}
}

func TestSortNames(t *testing.T) {
	got := []string{"libgit2@1.8", "libgit2-glib", "libgit2@1.10", "libgit2", "libgit2@1.7", "python@3.9", "python@3.13"}
	sortNames(got)
	want := []string{"libgit2", "libgit2@1.7", "libgit2@1.8", "libgit2@1.10", "libgit2-glib", "python@3.9", "python@3.13"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sortNames = %v, want %v", got, want)
	}
}
