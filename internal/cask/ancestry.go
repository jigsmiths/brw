package cask

import (
	"context"
	"os"
	"os/user"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const lsappinfoPath = "/usr/bin/lsappinfo"

var lsappinfoBundleID = regexp.MustCompile(`bundleID="([^"]*)"`)

func lsappinfoBundleIDs(out string) []string {
	var ids []string
	for _, m := range lsappinfoBundleID.FindAllStringSubmatch(out, -1) {
		ids = append(ids, m[1])
	}
	return ids
}

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

// bundleIDForPID asks LaunchServices, the registry behind NSRunningApplication, without needing Automation access.
func (in *Installer) bundleIDForPID(ctx context.Context, pid int) string {
	asn, err := in.exec(ctx, lsappinfoPath, "find", "pid="+strconv.Itoa(pid))
	if err != nil || strings.TrimSpace(string(asn)) == "" {
		return ""
	}
	out, err := in.exec(ctx, lsappinfoPath, "info", "-only", "bundleid", strings.TrimSpace(string(asn)))
	if err != nil {
		return ""
	}
	if ids := lsappinfoBundleIDs(string(out)); len(ids) > 0 {
		return ids[0]
	}
	return ""
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
	out, err := in.system(ctx, "who")
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
		out, _ := b.in.exec(ctx, lsappinfoPath, "list")
		b.running, b.listed = lsappinfoBundleIDs(string(out)), true
	}
	re := regexp.MustCompile(`(?i)^` + strings.ReplaceAll(regexp.QuoteMeta(id), `\*`, ".*") + `$`)
	return slices.DeleteFunc(slices.Clone(b.running), func(running string) bool { return !re.MatchString(running) })
}
