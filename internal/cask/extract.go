package cask

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type format int

const (
	plainFile format = iota
	zipFile
	tarFile
	dmgFile
	compressed
)

// Extract stages a download into dest: archives unpack (twice if they hold one archive), anything else is copied.
func Extract(ctx context.Context, file, dest string) error {
	if err := extractNested(ctx, file, dest); err != nil {
		return err
	}
	return propagateQuarantine(ctx, file, dest)
}

func extractNested(ctx context.Context, file, dest string) (err error) {
	kind, err := detect(ctx, file)
	if err != nil {
		return err
	}
	switch kind {
	case plainFile:
		return run(ctx, "cp", "-p", file, filepath.Join(dest, DownloadName(file)))
	case compressed:
		return fmt.Errorf("%s: a compressed file that is not a tar: %w", filepath.Base(file), ErrUnsupported)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".brw-unpack-*") //fsx:allow the caller's staging folder
	if err != nil {
		return err
	}
	defer func() {
		if rerr := os.RemoveAll(tmp); err == nil { //fsx:allow the caller's staging folder
			err = rerr
		}
	}()
	if err := unpack(ctx, kind, file, tmp); err != nil {
		return err
	}
	if err := run(ctx, "find", tmp, "-type", "d", "!", "-perm", "-u+w", "-exec", "chmod", "u+w", "{}", "+"); err != nil {
		return err
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	if len(entries) == 1 && entries[0].Type().IsRegular() {
		inner := filepath.Join(tmp, entries[0].Name())
		if k, err := detect(ctx, inner); err == nil && k != plainFile && k != compressed {
			return extractNested(ctx, inner, dest)
		}
	}
	for _, e := range entries {
		if err := os.Rename(filepath.Join(tmp, e.Name()), filepath.Join(dest, e.Name())); err != nil { //fsx:allow the caller's staging folder
			return err
		}
	}
	return nil
}

func unpack(ctx context.Context, kind format, file, dir string) error {
	switch kind {
	case zipFile:
		return unpackZip(ctx, file, dir)
	case tarFile:
		return run(ctx, "tar", "--extract", "--no-same-owner", "--file", file, "--directory", dir)
	case dmgFile:
		return unpackDmg(ctx, file, dir)
	}
	return fmt.Errorf("unpack %s: unknown format %d", file, kind)
}

func detect(ctx context.Context, file string) (format, error) {
	f, err := os.Open(file)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return 0, err
	}
	head = head[:n]
	packed := hasPrefix(head, "\x1f\x8b", "\xfd7zXZ\x00", "BZh", "\x28\xb5\x2f\xfd")
	switch {
	case len(head) >= 262 && string(head[257:262]) == "ustar":
		return tarFile, nil
	case packed && listsAsTar(ctx, file):
		return tarFile, nil
	case hasPrefix(head, "PK\x03\x04", "PK\x05\x06"):
		return zipFile, nil
	}
	// A bzip2-compressed image also starts like a bzip2 file, so the "koly" trailer is checked first.
	if fi, err := f.Stat(); err == nil && fi.Size() >= 512 {
		trailer := make([]byte, 4)
		if _, err := f.ReadAt(trailer, fi.Size()-512); err == nil && string(trailer) == "koly" {
			return dmgFile, nil
		}
	}
	if packed {
		return compressed, nil
	}
	return plainFile, nil
}

func listsAsTar(ctx context.Context, file string) bool {
	out, err := exec.CommandContext(ctx, "tar", "--list", "--file", file).Output()
	return err == nil && len(bytes.TrimSpace(out)) > 0
}

func hasPrefix(b []byte, magics ...string) bool {
	for _, m := range magics {
		if bytes.HasPrefix(b, []byte(m)) {
			return true
		}
	}
	return false
}

func unpackZip(ctx context.Context, file, dir string) error {
	if hasAppleDouble(file) {
		if _, err := exec.LookPath("ditto"); err == nil {
			return run(ctx, "ditto", "-x", "-k", file, dir)
		}
	}
	cmd := exec.CommandContext(ctx, "unzip", "-qq", "-o", file, "-d", dir)
	cmd.Env = append(os.Environ(), "TZ=UTC")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("unzip %s: %w: %s", file, err, bytes.TrimSpace(out))
	}
	return os.RemoveAll(filepath.Join(dir, "__MACOSX")) //fsx:allow the caller's staging folder
}

func hasAppleDouble(file string) bool {
	r, err := zip.OpenReader(file)
	if err != nil {
		return false
	}
	defer func() { _ = r.Close() }()
	for _, f := range r.File {
		if strings.HasPrefix(f.Name, "__MACOSX/") || strings.HasPrefix(filepath.Base(f.Name), "._") {
			return true
		}
	}
	return false
}

var dmgSkipped = map[string]bool{
	".background": true, ".com.apple.timemachine.donotpresent": true, ".com.apple.timemachine.supported": true,
	".DocumentRevisions-V100": true, ".DS_Store": true, ".fseventsd": true, ".MobileBackups": true,
	".Spotlight-V100": true, ".TemporaryItems": true, ".Trashes": true, ".VolumeIcon.icns": true,
	".HFS+ Private Directory Data\r": true, ".HFS+ Private Data\r": true,
}

var systemDirs = map[string]bool{
	"/Applications": true, "/Applications/Utilities": true, "/Library": true, "/System": true,
	"/Users": true, "/Volumes": true, "/bin": true, "/etc": true, "/opt": true, "/private": true,
	"/sbin": true, "/tmp": true, "/usr": true, "/var": true,
}

func unpackDmg(ctx context.Context, file, dir string) (err error) {
	mnt, err := os.MkdirTemp("", "brw-dmg-*") //fsx:allow mount point outside the prefix
	if err != nil {
		return err
	}
	defer func() {
		_ = os.Remove(filepath.Join(mnt, "mount")) //fsx:allow mount point outside the prefix
		_ = os.Remove(mnt)                         //fsx:allow mount point outside the prefix
	}()
	var out []byte
	var diskutilErr error
	eject := detach
	if diskutilImages() {
		if out, diskutilErr = attachDiskutil(ctx, file, mnt); diskutilErr == nil {
			eject = ejectDiskutil
		}
	}
	if out == nil {
		if out, err = attachHdiutil(ctx, file, mnt); err != nil {
			if diskutilErr != nil && !errors.Is(err, ErrUnsupported) {
				err = fmt.Errorf("%w (after %w)", err, diskutilErr)
			}
			return err
		}
	}
	mounts := mountPoints(out)
	dev := imageDevice(out)
	defer func() {
		for _, m := range mounts {
			err = errors.Join(err, eject(m, dev))
		}
		if len(mounts) == 0 {
			err = errors.Join(err, release(dev, nil))
		}
	}()
	if len(mounts) == 0 {
		return fmt.Errorf("no mounts found in '%s'; perhaps this is a bad disk image?", file)
	}
	for _, m := range mounts {
		if err := copyVolume(ctx, m, dir); err != nil {
			return err
		}
	}
	return run(ctx, "chmod", "-R", "u+w", dir)
}

// attachDiskutil fails on an image with a licence agreement, which it cancels with nothing to read.
func attachDiskutil(ctx context.Context, file, mnt string) ([]byte, error) {
	mount := filepath.Join(mnt, "mount")
	if err := os.Mkdir(mount, 0o755); err != nil { //fsx:allow mount point outside the prefix
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "diskutil", "image", "attach", "--plist", "--readOnly", "--nobrowse", "--mountPoint", mount, file)
	cmd.Env = append(os.Environ(), "PAGER=true")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		_ = os.Remove(mount) //fsx:allow mount point outside the prefix
		return nil, fmt.Errorf("diskutil image attach %s: %w: %s", file, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}

func attachHdiutil(ctx context.Context, file, mnt string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "hdiutil", "attach", "-plist", "-nobrowse", "-readonly", "-mountrandom", mnt, file)
	cmd.Stdin = strings.NewReader("qn\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if len(bytes.TrimSpace(out)) > 0 {
			return nil, fmt.Errorf("%s: a disk image with a licence agreement: %w", filepath.Base(file), ErrUnsupported)
		}
		return nil, fmt.Errorf("hdiutil attach %s: %w: %s", file, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}

var (
	runTool        = func(name string, args ...string) ([]byte, error) { return exec.Command(name, args...).CombinedOutput() }
	isMounted      = mounted
	deviceAttached = imageListed
	pause          = time.Sleep
)

const (
	detachTries = 3
	// Teardown lagged a passing test by more than a second, so allow a few.
	settleInterval = 250 * time.Millisecond
	settleTimeout  = 5 * time.Second
)

// imageListed asks hdiutil, which can still list an image after its /dev node is gone.
func imageListed(dev string) bool {
	if out, err := runTool("hdiutil", "info", "-plist"); err == nil {
		return slices.Contains(plistStrings(out, "dev-entry"), "/dev/"+dev)
	}
	_, err := os.Stat("/dev/" + dev)
	return err == nil
}

func imageDevice(plist []byte) string {
	devs := plistStrings(plist, "dev-entry")
	if len(devs) == 0 {
		return ""
	}
	return strings.TrimPrefix(devs[0], "/dev/")
}

// Unlike brew, which stops once the volume is unmounted, a leftover image is detached by device.
func ejectDiskutil(mount, dev string) error { return release(dev, ejectVolume(mount)) }

func detach(mount, dev string) error { return release(dev, detachVolume(mount)) }

func release(dev string, err error) error {
	if dev == "" {
		return err
	}
	for try := range detachTries {
		if !deviceAttached(dev) {
			return nil
		}
		args := []string{"detach", "-quiet", "/dev/" + dev}
		if try == detachTries-1 {
			args = []string{"detach", "-quiet", "-force", "/dev/" + dev}
		}
		out, derr := runTool("hdiutil", args...)
		if derr != nil {
			err = errors.Join(err, fmt.Errorf("hdiutil detach %s: %w: %s", dev, derr, bytes.TrimSpace(out)))
			pause(time.Second)
		} else {
			awaitUnlisted(dev)
		}
	}
	if deviceAttached(dev) {
		return errors.Join(err, fmt.Errorf("the disk image is still attached as %s", dev))
	}
	return nil
}

// awaitUnlisted gives a detach that returned success time to drop the device from hdiutil's listing.
func awaitUnlisted(dev string) {
	for waited := time.Duration(0); waited < settleTimeout && deviceAttached(dev); waited += settleInterval {
		pause(settleInterval)
	}
}

func ejectVolume(mount string) error {
	var err error
	for try := range 3 {
		if !isMounted(mount) {
			return nil
		}
		if try < 2 {
			err = ejectStores(mount)
		} else {
			err = diskutil("unmount", "force", mount)
		}
		if err == nil {
			return nil
		}
		pause(time.Second)
	}
	return err
}

// mounted is whether a volume is mounted at path: its device differs from its parent's.
func mounted(path string) bool {
	fi, err := os.Stat(path)
	parent, perr := os.Stat(filepath.Dir(path))
	if err != nil || perr != nil {
		return false
	}
	a, okA := fi.Sys().(*syscall.Stat_t)
	b, okB := parent.Sys().(*syscall.Stat_t)
	return okA && okB && a.Dev != b.Dev
}

func ejectStores(mount string) error {
	info, err := runTool("diskutil", "info", "-plist", mount)
	if err != nil {
		return fmt.Errorf("diskutil info %s: %w", mount, err)
	}
	stores := plistStrings(info, "APFSPhysicalStore")
	if len(stores) == 0 {
		stores = []string{mount}
	}
	for _, s := range stores {
		if err := diskutil("eject", s); err != nil {
			return err
		}
	}
	return nil
}

func diskutil(args ...string) error {
	if out, err := runTool("diskutil", args...); err != nil {
		return fmt.Errorf("diskutil %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

func copyVolume(ctx context.Context, volume, dir string) error {
	entries, err := os.ReadDir(volume)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(volume, e.Name())
		if dmgSkipped[e.Name()] {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(p); err == nil && systemDirs[filepath.Clean(target)] {
				continue
			}
		}
		if err := run(ctx, "ditto", p, filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func detachVolume(mount string) error {
	var err error
	for try := range 3 {
		if !isMounted(mount) {
			return nil
		}
		args := []string{"detach", "-quiet", mount}
		if try == 2 {
			args = []string{"detach", "-quiet", "-force", mount}
		}
		var out []byte
		if out, err = runTool("hdiutil", args...); err == nil {
			return nil
		}
		err = fmt.Errorf("hdiutil detach %s: %w: %s", mount, err, bytes.TrimSpace(out))
		pause(time.Second)
	}
	return err
}

func mountPoints(plist []byte) []string { return plistStrings(plist, "mount-point") }

func plistStrings(plist []byte, key string) []string {
	var mounts []string
	d := xml.NewDecoder(bytes.NewReader(plist))
	d.Strict = false
	var element, lastKey string
	for {
		tok, err := d.Token()
		if err != nil {
			return mounts
		}
		switch t := tok.(type) {
		case xml.StartElement:
			element = t.Name.Local
		case xml.CharData:
			switch element {
			case "key":
				lastKey = string(t)
			case "string":
				if lastKey == key {
					mounts = append(mounts, string(t))
					lastKey = ""
				}
			}
		case xml.EndElement:
			element = ""
		}
	}
}

// diskutilImages is true from macOS 27.2, where hdiutil attach is deprecated and fails now and then.
var diskutilImages = sync.OnceValue(func() bool {
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	return err == nil && macAtLeast(strings.TrimSpace(string(out)), 27, 2)
})

func macAtLeast(version string, major, minor int) bool {
	parts := strings.Split(version, ".")
	ma, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	mi := 0
	if len(parts) > 1 {
		mi, _ = strconv.Atoi(parts[1])
	}
	return ma > major || ma == major && mi >= minor
}

func run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}
