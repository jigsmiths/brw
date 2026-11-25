package cask

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jigsmiths/brw/internal/sudo"
)

func (in *Installer) sameApp(ctx context.Context, source, target string) (bool, error) {
	sShort, sVersion, sok, err := in.bundleVersion(ctx, filepath.Join(source, "Contents/Info.plist"))
	if err != nil {
		return false, err
	}
	var tShort, tVersion string
	tok := false
	if sok {
		if tShort, tVersion, tok, err = in.bundleVersion(ctx, filepath.Join(target, "Contents/Info.plist")); err != nil {
			return false, err
		}
	}
	if !sok || !tok {
		_, err := in.system(ctx, "/usr/bin/diff", "--recursive", "--brief", source, target)
		return err == nil, nil
	}
	switch {
	case sShort != tShort:
		in.errorf("The bundle short version of %s is %s but is %s for %s!", source, sShort, tShort, target)
		return false, nil
	case sVersion != tVersion:
		in.errorf("The bundle version of %s is %s but is %s for %s!", source, sVersion, tVersion, target)
		return false, nil
	}
	return true, nil
}

// bundleVersion is BundleVersion.from_info_plist; a plutil that fails raises, as system_command! does.
func (in *Installer) bundleVersion(ctx context.Context, plist string) (short, version string, ok bool, err error) {
	if fi, err := os.Stat(plist); err != nil || fi.Size() == 0 {
		return "", "", false, nil
	}
	out, err := in.capture(ctx, in.command(sudo.User, "/usr/bin/plutil", "-convert", "xml1", "-o", "-", plist))
	if err != nil {
		return "", "", false, err
	}
	values, err := plistDictStrings([]byte(out))
	if err != nil {
		return "", "", false, nil
	}
	short, version = strings.TrimSpace(values["CFBundleShortVersionString"]), strings.TrimSpace(values["CFBundleVersion"])
	if version != "" {
		short = strings.TrimSpace(strings.TrimSuffix(short, "("+version+")"))
	}
	return short, version, short != "" || version != "", nil
}

func (in *Installer) errorf(format string, args ...any) {
	if in.Warn != nil {
		_, _ = fmt.Fprintf(in.Warn, "Error: "+format+"\n", args...)
	}
}
