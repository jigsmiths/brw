package main

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/jigsmiths/brw/internal/cellar"
)

// armFamilies are Hardware::CPU.arm_family's names for hw.cpufamily.
var armFamilies = map[uint32]string{
	0x2c91a47e: "arm_typhoon", 0x92fb37c8: "arm_twister", 0x67ceee93: "arm_hurricane_zephyr",
	0xe81e7ef6: "arm_monsoon_mistral", 0x07d34b9f: "arm_vortex_tempest", 0x462504d2: "arm_lightning_thunder",
	0x573b5eec: "arm_firestorm_icestorm", 0x1b588bb3: "arm_firestorm_icestorm", 0xda33d83d: "arm_blizzard_avalanche",
	0xfa33415e: "arm_ibiza", 0x5f4dea93: "arm_lobos", 0x72015832: "arm_palma", 0x6f5129ac: "arm_donan",
	0x17d5b93a: "arm_brava", 0x1d5a87e8: "arm_hidra", 0xf76c5b1a: "arm_sotra",
}

// sdkPath is the macOS SDK builds compile against, as xcrun finds it.
func sdkPath() string {
	out, err := exec.Command("/usr/bin/xcrun", "--show-sdk-path").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// buildSystem is brew's DevelopmentTools.build_system_info on this Mac, which
// a receipt of a formula built here records.
func buildSystem() cellar.BuiltOn {
	v := macVersion()
	if i := strings.LastIndex(v, "."); strings.Count(v, ".") == 2 && i > 0 {
		v = v[:i] // brew drops the patch version
	}
	on := cellar.BuiltOn{OS: "Macintosh", OSVersion: "macOS " + v, CPUFamily: "dunno", PreferredPerl: "5.30"}
	if major, err := strconv.Atoi(strings.SplitN(v, ".", 2)[0]); err == nil && major >= 14 {
		on.PreferredPerl = "5.34"
	}
	if fam, err := syscall.SysctlUint32("hw.cpufamily"); err == nil {
		if name, ok := armFamilies[fam]; ok {
			on.CPUFamily = name
		}
	}
	if out, err := exec.Command("/usr/sbin/pkgutil", "--pkg-info=com.apple.pkg.CLTools_Executables").Output(); err == nil {
		for line := range strings.SplitSeq(string(out), "\n") {
			if rest, ok := strings.CutPrefix(line, "version: "); ok {
				on.CLT = strings.TrimSpace(rest)
			}
		}
	}
	if dir, err := exec.Command("/usr/bin/xcode-select", "-p").Output(); err == nil {
		if app, _, ok := strings.Cut(strings.TrimSpace(string(dir)), ".app/"); ok {
			plist := filepath.Join(app+".app", "Contents", "version.plist")
			if out, err := exec.Command("/usr/bin/plutil", "-extract", "CFBundleShortVersionString", "raw", plist).Output(); err == nil {
				on.Xcode = strings.TrimSpace(string(out))
			}
		}
	}
	return on
}

// macVersion is the macOS product version, such as "27.0.1". Reading it from
// the kernel avoids running sw_vers, which costs about 16ms.
func macVersion() string {
	v, err := syscall.Sysctl("kern.osproductversion")
	if err != nil {
		return ""
	}
	return v
}
