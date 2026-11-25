package cask

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const appExecutableDir = ".app/Contents/MacOS/"

const runningBundleIDsScript = `'use strict';

ObjC.import('AppKit')

function run() {
  var apps = $.NSWorkspace.sharedWorkspace.runningApplications
  var bundleIds = []

  for (var i = 0; i < apps.count; i++) {
    var bundleId = apps.objectAtIndex(i).bundleIdentifier
    if (!bundleId.isNil()) {
      bundleIds.push(ObjC.unwrap(bundleId))
    }
  }

  return bundleIds.join("\n")
}`

func (in *Installer) hostingBrw(ctx context.Context, bundleID string) bool {
	if !in.ancestorsKnown {
		in.ancestors, in.ancestorsKnown = in.ancestorBundleIDs(ctx), true
	}
	return slices.ContainsFunc(in.ancestors, func(id string) bool { return strings.EqualFold(id, bundleID) })
}

func (in *Installer) ancestorBundleIDs(ctx context.Context) []string {
	pids := []int{os.Getpid()}
	for {
		ppid, ok := in.parentPID(ctx, pids[len(pids)-1])
		if !ok || ppid <= 1 || slices.Contains(pids, ppid) {
			break
		}
		pids = append(pids, ppid)
	}
	var ids []string
	for _, pid := range pids {
		if id := in.bundleIDForPID(ctx, pid); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// bundleIDForPID reads the bundle ID from the app that holds the process's executable, which needs no Automation access.
func (in *Installer) bundleIDForPID(ctx context.Context, pid int) string {
	out, err := in.exec(ctx, "/bin/ps", "-o", "comm=", "-p", strconv.Itoa(pid))
	if err != nil {
		return ""
	}
	bundle := appBundleOf(strings.TrimSpace(string(out)))
	if bundle == "" {
		return ""
	}
	out, err = in.exec(ctx, "/usr/bin/plutil", "-extract", "CFBundleIdentifier", "raw", "-o", "-", filepath.Join(bundle, "Contents/Info.plist"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// appBundleOf is the innermost app bundle whose main executable is comm.
func appBundleOf(comm string) string {
	i := strings.LastIndex(comm, appExecutableDir)
	if i < 0 {
		return ""
	}
	return comm[:i+len(".app")]
}

func (in *Installer) parentPID(ctx context.Context, pid int) (int, bool) {
	out, err := in.exec(ctx, "/bin/ps", "-o", "ppid=", "-p", strconv.Itoa(pid))
	if err != nil {
		return 0, false
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	return ppid, err == nil
}

func (in *Installer) gui(ctx context.Context) bool {
	u, err := user.Current()
	if err != nil {
		return false
	}
	out, err := in.exec(ctx, "who")
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[0] == u.Username && f[1] == "console" {
			return true
		}
	}
	return false
}

// bundleIDs expands `*` in bundle IDs to the running applications that match, listing them once.
type bundleIDs struct {
	in      *Installer
	running []string
	listed  bool
}

func (b *bundleIDs) expand(ctx context.Context, id string) []string {
	if !strings.Contains(id, "*") {
		return []string{id}
	}
	if !b.in.gui(ctx) {
		b.in.warnf("Not logged into a GUI; skipping applications matching '%s'.", id)
		return nil
	}
	if !b.listed {
		out, _ := b.in.exec(ctx, "osascript", "-l", "JavaScript", "-e", runningBundleIDsScript)
		for running := range strings.SplitSeq(string(out), "\n") {
			if running = strings.TrimSpace(running); running != "" {
				b.running = append(b.running, running)
			}
		}
		b.listed = true
	}
	re := regexp.MustCompile(`(?i)^` + strings.ReplaceAll(regexp.QuoteMeta(id), `\*`, ".*") + `$`)
	return slices.DeleteFunc(slices.Clone(b.running), func(running string) bool { return !re.MatchString(running) })
}
