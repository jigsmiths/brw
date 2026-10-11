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
