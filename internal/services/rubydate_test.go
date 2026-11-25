package services

import (
	"os"
	"strings"
	"testing"
	"time"
)

func unescapeTSV(s string) string {
	return strings.NewReplacer(`\\`, "\x00", `\n`, "\n", `\t`, "\t", `\r`, "\r", `\v`, "\v", `\f`, "\f").Replace(s)
}

// testdata/ruby_plists.tsv is brew's Plist.parse_xml(marshal: false) plus UserName and to_plist, one case per line.
func TestApplyUserNameMatchesRubyPlistGem(t *testing.T) {
	body, err := os.ReadFile("testdata/ruby_plists.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(body), "\n"), "\n") {
		in, want, _ := strings.Cut(line, "\t")
		in, want = strings.ReplaceAll(unescapeTSV(in), "\x00", `\`), strings.ReplaceAll(unescapeTSV(want), "\x00", `\`)
		got, err := ApplyUserName([]byte(in), "svc")
		if err != nil {
			got = []byte("ERR")
		}
		if string(got) != want {
			t.Errorf("input %q:\n got %q\nwant %q", in, got, want)
		}
	}
}

// testdata/ruby_dates.tsv is DateTime.parse(input).strftime output from brew's Ruby on 2026-11-25.
func TestRubyDateTimeMatchesRuby(t *testing.T) {
	body, err := os.ReadFile("testdata/ruby_dates.tsv")
	if err != nil {
		t.Fatal(err)
	}
	today := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(lines) < 100 {
		t.Fatalf("only %d fixtures", len(lines))
	}
	for _, line := range lines {
		input, want, _ := strings.Cut(line, "\t")
		got, err := rubyDateTime(input, today)
		if err != nil {
			got = "ERR"
		}
		if got != want {
			t.Errorf("%q: got %s, Ruby says %s", input, got, want)
		}
	}
}
