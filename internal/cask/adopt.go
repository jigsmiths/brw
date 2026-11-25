package cask

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (in *Installer) sameApp(ctx context.Context, source, target string) bool {
	sShort, sVersion, sok := in.bundleVersion(ctx, filepath.Join(source, "Contents/Info.plist"))
	tShort, tVersion, tok := in.bundleVersion(ctx, filepath.Join(target, "Contents/Info.plist"))
	if !sok || !tok {
		_, err := in.system(ctx, "/usr/bin/diff", "--recursive", "--brief", source, target)
		return err == nil
	}
	switch {
	case sShort != tShort:
		in.errorf("The bundle short version of %s is %s but is %s for %s!", source, sShort, tShort, target)
		return false
	case sVersion != tVersion:
		in.errorf("The bundle version of %s is %s but is %s for %s!", source, sVersion, tVersion, target)
		return false
	}
	return true
}

func (in *Installer) bundleVersion(ctx context.Context, plist string) (short, version string, ok bool) {
	if fi, err := os.Stat(plist); err != nil || fi.Size() == 0 {
		return "", "", false
	}
	out, err := in.system(ctx, "/usr/bin/plutil", "-convert", "xml1", "-o", "-", plist)
	if err != nil {
		return "", "", false
	}
	values, err := plistDictStrings(out)
	if err != nil {
		return "", "", false
	}
	short, version = strings.TrimSpace(values["CFBundleShortVersionString"]), strings.TrimSpace(values["CFBundleVersion"])
	if version != "" {
		short = strings.TrimSpace(strings.TrimSuffix(short, "("+version+")"))
	}
	return short, version, short != "" || version != ""
}

func (in *Installer) errorf(format string, args ...any) {
	if in.Warn != nil {
		_, _ = fmt.Fprintf(in.Warn, "Error: "+format+"\n", args...)
	}
}
