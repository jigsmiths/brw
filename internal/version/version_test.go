package version

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func mustParse(t *testing.T, s string) Version {
	t.Helper()
	v, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return v
}

// compare.json holds results from Homebrew's own Version#<=>.
func TestMatchesBrew(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "compare.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pairs []struct {
		A, B string
		Cmp  int
	}
	if err := json.Unmarshal(b, &pairs); err != nil {
		t.Fatal(err)
	}
	if len(pairs) < 2000 {
		t.Fatalf("only %d oracle pairs", len(pairs))
	}
	bad := 0
	for _, p := range pairs {
		if got := mustParse(t, p.A).Compare(mustParse(t, p.B)); got != p.Cmp {
			bad++
			if bad <= 10 {
				t.Errorf("Compare(%q, %q) = %d, brew says %d", p.A, p.B, got, p.Cmp)
			}
		}
	}
	if bad > 10 {
		t.Errorf("%d of %d pairs differ from brew", bad, len(pairs))
	}
}

func TestOrdering(t *testing.T) {
	ascending := []string{"1.0alpha", "1.0alpha2", "1.0beta", "1.0beta2", "1.0rc1", "1.0rc2", "1.0"}
	for i := range ascending {
		for j := range ascending {
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := mustParse(t, ascending[i]).Compare(mustParse(t, ascending[j])); got != want {
				t.Errorf("Compare(%q, %q) = %d, want %d", ascending[i], ascending[j], got, want)
			}
		}
	}
	for _, tt := range []struct {
		a, b string
		want int
	}{
		{"1.0", "1.0.0", 0},
		{"1.9", "1.10", -1},
		{"2", "1.99", 1},
		{"HEAD", "9.9", 1},
		{"HEAD", "HEAD-abc", 0},
	} {
		if got := mustParse(t, tt.a).Compare(mustParse(t, tt.b)); got != tt.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestParseEmpty(t *testing.T) {
	if _, err := Parse(""); err == nil {
		t.Error("expected error")
	}
}

func TestString(t *testing.T) {
	if got := mustParse(t, "1.0rc1").String(); got != "1.0rc1" {
		t.Errorf("String() = %q", got)
	}
}

func TestPkgVersion(t *testing.T) {
	tests := []struct {
		in       string
		version  string
		revision int
		str      string
	}{
		{"1.2", "1.2", 0, "1.2"},
		{"1.2_3", "1.2", 3, "1.2_3"},
		{"1.2_0", "1.2", 0, "1.2"},
		{"foo_bar", "foo_bar", 0, "foo_bar"},
		{"1_2_3", "1_2", 3, "1_2_3"},
	}
	for _, tt := range tests {
		p, err := ParsePkg(tt.in)
		if err != nil {
			t.Fatalf("ParsePkg(%q): %v", tt.in, err)
		}
		if p.Version.String() != tt.version || p.Revision != tt.revision || p.String() != tt.str {
			t.Errorf("ParsePkg(%q) = %q rev %d (%q), want %q rev %d (%q)",
				tt.in, p.Version, p.Revision, p, tt.version, tt.revision, tt.str)
		}
	}
	if _, err := ParsePkg(""); err == nil {
		t.Error("ParsePkg(\"\") should fail")
	}
}

func TestPkgVersionCompare(t *testing.T) {
	pk := func(s string) PkgVersion {
		p, err := ParsePkg(s)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tt := range []struct {
		a, b string
		want int
	}{
		{"1.0_1", "1.0", 1},
		{"1.0_2", "1.0_1", 1},
		{"1.1", "1.0_9", 1},
		{"1.0_0", "1.0", 0},
		{"1.0", "1.0.0_1", -1},
	} {
		if got := pk(tt.a).Compare(pk(tt.b)); got != tt.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
	p, err := NewPkg("1.0", 2)
	if err != nil || p.String() != "1.0_2" {
		t.Errorf("NewPkg = %q, %v", p, err)
	}
}
