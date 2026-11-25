package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/fsx"
)

const packageName = `[\w+\-.@]+`

var (
	jobLabel     = regexp.MustCompile(`(?:homebrew(?:\.mxcl)?|sh\.brew)\.` + packageName + `(?:\s|$)`)
	serviceLabel = regexp.MustCompile(`(?:homebrew(?:\.mxcl)?|sh\.brew)\.(` + packageName + `)$`)
	serviceExt   = regexp.MustCompile(`(?i)\.(?:plist|service|timer)$`)
)

var serviceFilePatterns = func() []string {
	var out []string
	for _, prefix := range []string{"homebrew.*", "sh.brew.*"} {
		for _, ext := range []string{"plist", "service", "timer"} {
			out = append(out, prefix+"."+ext)
		}
	}
	return out
}()

func runningLabels(jobs string) []string {
	var labels []string
	for line := range strings.SplitSeq(strings.TrimSuffix(jobs, "\n"), "\n") {
		if m := jobLabel.FindString(line); m != "" {
			labels = append(labels, strings.TrimSuffix(strings.TrimSpace(m), ".service"))
		}
	}
	return labels
}

func (e *env) servicesCleanup(o *servicesOpts, prefix string) error {
	killed, err := e.killOrphanedServices(prefix)
	if err != nil {
		return err
	}
	removed, err := e.removeUnusedServiceFiles(o.journal)
	if err != nil {
		return err
	}
	if killed+removed > 0 {
		return nil
	}
	kind := "user-space"
	if e.root() {
		kind = "root"
	}
	_, err = fmt.Fprintf(e.out, "All %s services OK, nothing cleaned...\n", kind)
	return err
}

func (e *env) killOrphanedServices(prefix string) (int, error) {
	kegs, err := e.kegs(prefix)
	if err != nil {
		return 0, err
	}
	jobs, err := e.launchctl.Jobs()
	if err != nil {
		return 0, err
	}
	var orphans []serviceTarget
	for _, label := range runningLabels(jobs) {
		m := serviceLabel.FindStringSubmatch(strings.TrimSuffix(label, ".plist"))
		if m == nil {
			e.opoo("Service %s not managed by `brw services` => skipping", label)
			continue
		}
		t, _ := e.installedService(prefix, kegs, m[1])
		if !t.installed {
			var known bool
			if t, known = e.apiTarget(prefix, m[1]); !known {
				e.opoo("Service %s not managed by `brw services` => skipping", label)
				continue
			}
		}
		t.explicit = label
		if fi, err := os.Stat(filepath.Join(e.destDir(), label+".plist")); err == nil && fi.Mode().IsRegular() {
			continue
		}
		orphans = append(orphans, t)
	}
	return len(orphans), e.servicesKill(orphans)
}

func (e *env) removeUnusedServiceFiles(journal string) (int, error) {
	entries, err := os.ReadDir(e.destDir())
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	jobs, err := e.launchctl.Jobs()
	if err != nil {
		return 0, err
	}
	running := runningLabels(jobs)
	removed := 0
	for _, pattern := range serviceFilePatterns {
		for _, entry := range entries {
			if ok, _ := filepath.Match(pattern, entry.Name()); !ok {
				continue
			}
			path := filepath.Join(e.destDir(), entry.Name())
			name, label := serviceExt.ReplaceAllString(entry.Name(), ""), ""
			if strings.EqualFold(filepath.Ext(path), ".plist") {
				label, _ = e.fileLabel(path)
				if label != "" {
					name = label
				}
			}
			if slices.Contains(running, name) {
				continue
			}
			if entry.IsDir() {
				_, _ = fmt.Fprintf(e.out, "Removing unused service file: %s\n", path)
				return removed, errors.New("Operation not permitted @ apply2files - " + path) //nolint:staticcheck // matches ruby's error text
			}
			if label != "" {
				if up, err := e.running(label); err != nil {
					return removed, err
				} else if up {
					continue
				}
			}
			_, _ = fmt.Fprintf(e.out, "Removing unused service file: %s\n", path)
			err := withRoot(e.destDir(), journal, func(root *fsx.Root) error { return root.Remove(entry.Name()) })
			if err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}
