package cask

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

const fakeDev = "disk7"

type fakeDisk struct {
	mounted, attached bool
	calls             []string
}

func stubDisk(t *testing.T, d *fakeDisk) {
	t.Helper()
	oldRun, oldMounted, oldAttached, oldPause := runTool, isMounted, deviceAttached, pause
	t.Cleanup(func() { runTool, isMounted, deviceAttached, pause = oldRun, oldMounted, oldAttached, oldPause })
	isMounted = func(string) bool { return d.mounted }
	deviceAttached = func(dev string) bool { return dev == fakeDev && d.attached }
	pause = func(time.Duration) {}
	runTool = func(name string, args ...string) ([]byte, error) {
		d.calls = append(d.calls, name+" "+strings.Join(args, " "))
		switch {
		case name == "diskutil" && args[0] == "info":
			return []byte("<plist><dict><key>APFSPhysicalStore</key><string>disk7s1</string></dict></plist>"), nil
		case name == "diskutil" && args[0] == "eject", name == "hdiutil" && !slices.Contains(args, "-force"):
			d.mounted = false
			return []byte("Resource busy"), errors.New("exit status 1")
		case name == "hdiutil" && slices.Contains(args, "-force") && args[len(args)-1] == "/dev/"+fakeDev:
			d.attached = false
		}
		return nil, nil
	}
}

func TestEjectDiskutilDetachesAfterAFailedEject(t *testing.T) {
	d := &fakeDisk{mounted: true, attached: true}
	stubDisk(t, d)
	if err := ejectDiskutil("/mnt/mount", fakeDev); err != nil {
		t.Errorf("err = %v", err)
	}
	if d.attached {
		t.Errorf("the image is still attached after %q", d.calls)
	}
}

func TestDetachDetachesAfterAFailedDetach(t *testing.T) {
	d := &fakeDisk{mounted: true, attached: true}
	stubDisk(t, d)
	if err := detach("/mnt/mount", fakeDev); err != nil {
		t.Errorf("err = %v", err)
	}
	if d.attached {
		t.Errorf("the image is still attached after %q", d.calls)
	}
}

const listedDev = "disk99999"

func TestReleaseDetachesAnImageThatHdiutilStillListsWithoutADeviceNode(t *testing.T) {
	oldRun, oldPause := runTool, pause
	t.Cleanup(func() { runTool, pause = oldRun, oldPause })
	pause = func(time.Duration) {}
	listed := true
	var calls []string
	runTool = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "hdiutil" && args[0] == "info" && listed {
			return []byte("<plist><dict><key>dev-entry</key><string>/dev/" + listedDev + "</string></dict></plist>"), nil
		}
		if name == "hdiutil" && args[0] == "detach" {
			listed = false
		}
		return nil, nil
	}
	if err := release(listedDev, nil); err != nil || listed {
		t.Errorf("err = %v, still listed = %v after %q", err, listed, calls)
	}
}

type lingeringImage struct {
	detaches, lingers int
	detached          bool
	pauses            []time.Duration
}

// stubLingering makes hdiutil detach succeed while info keeps listing the device for lingers more calls.
func stubLingering(t *testing.T, l *lingeringImage) {
	t.Helper()
	oldRun, oldPause := runTool, pause
	t.Cleanup(func() { runTool, pause = oldRun, oldPause })
	pause = func(d time.Duration) { l.pauses = append(l.pauses, d) }
	runTool = func(name string, args ...string) ([]byte, error) {
		switch {
		case name == "hdiutil" && args[0] == "detach":
			l.detaches++
			l.detached = true
		case name == "hdiutil" && args[0] == "info":
			if l.detached {
				if l.lingers == 0 {
					return []byte("<plist/>"), nil
				}
				l.lingers--
			}
			return []byte("<plist><dict><key>dev-entry</key><string>/dev/" + listedDev + "</string></dict></plist>"), nil
		}
		return nil, nil
	}
}

func TestReleaseWaitsForTheListingToClearAfterADetach(t *testing.T) {
	l := &lingeringImage{lingers: 3}
	stubLingering(t, l)
	if err := release(listedDev, nil); err != nil {
		t.Errorf("err = %v", err)
	}
	if l.detaches != 1 || len(l.pauses) != 3 {
		t.Errorf("detaches = %d, pauses = %v, want 1 detach and 3 pauses", l.detaches, l.pauses)
	}
}

func TestReleaseFailsWhenTheListingNeverClears(t *testing.T) {
	l := &lingeringImage{lingers: 1 << 20}
	stubLingering(t, l)
	if err := release(listedDev, nil); err == nil {
		t.Error("err = nil, want the image still attached")
	}
	if len(l.pauses) == 0 || time.Duration(len(l.pauses))*settleInterval > detachTries*settleTimeout {
		t.Errorf("pauses = %d of %v, want a bounded wait", len(l.pauses), settleInterval)
	}
}

func TestReleaseDoesNotWaitWhenTheListingClearsAtOnce(t *testing.T) {
	l := &lingeringImage{}
	stubLingering(t, l)
	if err := release(listedDev, nil); err != nil || len(l.pauses) != 0 {
		t.Errorf("err = %v, pauses = %v, want none", err, l.pauses)
	}
}

func TestDetachIgnoresAnImageThatIsGone(t *testing.T) {
	d := &fakeDisk{}
	stubDisk(t, d)
	if err := ejectDiskutil("/mnt/mount", fakeDev); err != nil || len(d.calls) != 0 {
		t.Errorf("err = %v, calls = %q, want none", err, d.calls)
	}
}
