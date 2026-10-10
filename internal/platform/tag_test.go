package platform

import "testing"

func TestTag(t *testing.T) {
	tests := []struct {
		name    string
		host    Host
		want    string
		wantErr bool
	}{
		{"macOS 27 arm64", Host{OS: "darwin", MacVersion: "27.0.1", Arch: "arm64"}, "arm64_golden_gate", false},
		{"macOS 26 arm64", Host{OS: "darwin", MacVersion: "26.4", Arch: "arm64"}, "arm64_tahoe", false},
		{"macOS 15 intel", Host{OS: "darwin", MacVersion: "15.1", Arch: "amd64"}, "sequoia", false},
		{"macOS 14 arm64", Host{OS: "darwin", MacVersion: "14.7.2", Arch: "arm64"}, "arm64_sonoma", false},
		{"macOS 13 intel", Host{OS: "darwin", MacVersion: "13.6", Arch: "amd64"}, "ventura", false},
		{"macOS 11 arm64", Host{OS: "darwin", MacVersion: "11.7", Arch: "arm64"}, "arm64_big_sur", false},
		{"macOS 10.15 unsupported", Host{OS: "darwin", MacVersion: "10.15.7", Arch: "amd64"}, "", true},
		{"macOS bad version", Host{OS: "darwin", MacVersion: "banana", Arch: "arm64"}, "", true},
		{"linux amd64", Host{OS: "linux", Arch: "amd64"}, "x86_64_linux", false},
		{"linux arm64", Host{OS: "linux", Arch: "arm64"}, "arm64_linux", false},
		{"linux 386 unsupported", Host{OS: "linux", Arch: "386"}, "", true},
		{"windows unsupported", Host{OS: "windows", Arch: "amd64"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Tag(tt.host)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Tag() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("Tag() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSelect(t *testing.T) {
	// Tags as seen in the real API for jq.
	jq := []string{
		"arm64_golden_gate", "arm64_tahoe", "arm64_sequoia", "arm64_sonoma",
		"sonoma", "arm64_linux", "x86_64_linux",
	}
	tests := []struct {
		name      string
		host      Host
		available []string
		want      string
		wantOK    bool
	}{
		{"exact match", Host{OS: "darwin", MacVersion: "27.0.1", Arch: "arm64"}, jq, "arm64_golden_gate", true},
		{"falls back to older macOS", Host{OS: "darwin", MacVersion: "26.0", Arch: "arm64"}, []string{"arm64_sequoia", "arm64_sonoma"}, "arm64_sequoia", true},
		{"unknown macOS has no match, like brew", Host{OS: "darwin", MacVersion: "28.0", Arch: "arm64"}, jq, "", false},
		{"unknown macOS still gets all bottle", Host{OS: "darwin", MacVersion: "28.0", Arch: "arm64"}, []string{"arm64_golden_gate", "all"}, "all", true},
		{"never picks newer macOS", Host{OS: "darwin", MacVersion: "14.0", Arch: "arm64"}, []string{"arm64_tahoe", "arm64_sequoia"}, "", false},
		{"never crosses arch", Host{OS: "darwin", MacVersion: "15.0", Arch: "amd64"}, []string{"arm64_sequoia"}, "", false},
		{"intel older fallback", Host{OS: "darwin", MacVersion: "15.0", Arch: "amd64"}, []string{"sonoma", "ventura"}, "sonoma", true},
		{"all bottle is last resort", Host{OS: "darwin", MacVersion: "27.0", Arch: "arm64"}, []string{"all"}, "all", true},
		{"exact beats all", Host{OS: "darwin", MacVersion: "26.0", Arch: "arm64"}, []string{"all", "arm64_tahoe"}, "arm64_tahoe", true},
		{"older beats all", Host{OS: "darwin", MacVersion: "27.0", Arch: "arm64"}, []string{"all", "arm64_tahoe"}, "arm64_tahoe", true},
		{"linux amd64", Host{OS: "linux", Arch: "amd64"}, jq, "x86_64_linux", true},
		{"linux arm64", Host{OS: "linux", Arch: "arm64"}, jq, "arm64_linux", true},
		{"linux does not use macOS tags", Host{OS: "linux", Arch: "amd64"}, []string{"arm64_tahoe", "sonoma"}, "", false},
		{"nothing available", Host{OS: "darwin", MacVersion: "27.0", Arch: "arm64"}, nil, "", false},
		{"unsupported host", Host{OS: "windows", Arch: "amd64"}, jq, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Select(tt.host, tt.available)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("Select() = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestReleaseMajor(t *testing.T) {
	for name, want := range map[string]int{"golden_gate": 27, "tahoe": 26, "ventura": 13, "big_sur": 11} {
		if got, ok := ReleaseMajor(name); !ok || got != want {
			t.Errorf("ReleaseMajor(%q) = %d, %v; want %d", name, got, ok, want)
		}
	}
	if _, ok := ReleaseMajor("catalina"); ok {
		t.Error("catalina is not supported")
	}
}

func TestMacMajor(t *testing.T) {
	if got, err := MacMajor("27.0.1"); err != nil || got != 27 {
		t.Errorf("MacMajor(27.0.1) = %d, %v", got, err)
	}
	for _, bad := range []string{"10.15", "banana", ""} {
		if _, err := MacMajor(bad); err == nil {
			t.Errorf("MacMajor(%q) should fail", bad)
		}
	}
}
