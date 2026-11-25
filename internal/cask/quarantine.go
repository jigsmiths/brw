package cask

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	quarantineAttr = "com.apple.quarantine"
	xattrTool      = "/usr/bin/xattr"
	// noTranslocation makes macOS run the app in place instead of from a read-only copy.
	noTranslocation = 0x0100
)

func quarantine(ctx context.Context, path string) error {
	if _, err := os.Stat(xattrTool); err != nil {
		return nil
	}
	if readQuarantine(ctx, path) != "" {
		return nil
	}
	value := fmt.Sprintf("0081;%08x;;%s", time.Now().Unix(), uuid())
	if out, err := exec.CommandContext(ctx, xattrTool, "-w", quarantineAttr, value, path).CombinedOutput(); err != nil {
		return fmt.Errorf("quarantine %s: %w: %s", path, err, bytes.TrimSpace(out))
	}
	return nil
}

func readQuarantine(ctx context.Context, path string) string {
	out, err := exec.CommandContext(ctx, xattrTool, "-p", quarantineAttr, path).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func propagated(value string) string {
	flags, rest, ok := strings.Cut(value, ";")
	n, err := strconv.ParseUint(flags, 16, 16)
	if !ok || err != nil {
		return ""
	}
	return fmt.Sprintf("%04x;%s", n|noTranslocation, rest)
}

func propagateQuarantine(ctx context.Context, download, dir string) error {
	if _, err := os.Stat(xattrTool); err != nil {
		return nil
	}
	value := propagated(readQuarantine(ctx, download))
	if value == "" {
		return nil
	}
	var paths bytes.Buffer
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir || d.Type()&fs.ModeSymlink != 0 {
			return err
		}
		paths.WriteString(p)
		paths.WriteByte(0)
		return nil
	})
	if err != nil || paths.Len() == 0 {
		return err
	}
	for _, args := range [][]string{{"/bin/chmod", "-h", "u+w"}, {xattrTool, "-w", quarantineAttr, value}} {
		cmd := exec.CommandContext(ctx, "/usr/bin/xargs", append([]string{"-0", "--"}, args...)...)
		cmd.Stdin = bytes.NewReader(paths.Bytes())
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to quarantine one or more files within %s: %w: %s", dir, err, bytes.TrimSpace(out))
		}
	}
	return nil
}

func uuid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]))
}

const (
	codesignTool = "/usr/bin/codesign"
	// userApproved is the flag macOS sets once the user has allowed an app to open.
	userApproved = 0x0040
	// requirementFailed is codesign's exit status when -R does not match.
	requirementFailed = 3
)

type approval struct {
	approved      bool
	unquarantined bool
	identity      string
}

func (in *Installer) quarantineAvailable() bool {
	_, err := os.Stat(xattrTool)
	return in.Host.OS == "darwin" && err == nil
}

func (in *Installer) approvals(ctx context.Context, apps []string) []approval {
	if !in.quarantineAvailable() {
		return nil
	}
	out := make([]approval, len(apps))
	for i, app := range apps {
		if _, err := os.Stat(app); err == nil {
			value := readQuarantine(ctx, app)
			out[i].unquarantined = value == ""
			flags, _, _ := strings.Cut(value, ";")
			n, err := strconv.ParseUint(flags, 16, 32)
			out[i].approved = err == nil && n&userApproved != 0
		}
		out[i].identity = in.signingIdentity(ctx, app)
	}
	return out
}

func (in *Installer) signingIdentity(ctx context.Context, app string) string {
	out, err := in.exec(ctx, codesignTool, "-d", "-r-", app)
	if err != nil {
		return ""
	}
	var req string
	for line := range strings.Lines(string(out)) {
		if r, ok := strings.CutPrefix(strings.TrimPrefix(line, "# "), "designated => "); ok {
			req = strings.TrimSpace(r)
		}
	}
	if req == "" {
		return ""
	}
	if match, ok := in.identityMatch(ctx, app, req); !ok || !match {
		return ""
	}
	return req
}

func (in *Installer) identityMatch(ctx context.Context, app, req string) (match, ok bool) {
	_, err := in.exec(ctx, codesignTool, "--verify", "--deep", "--strict", "--all-architectures", "-R", "="+req, app)
	if err == nil {
		return true, true
	}
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) && exit.ExitCode() == requirementFailed {
		return false, true
	}
	return false, false
}

func (in *Installer) releaseQuarantine(ctx context.Context, token string, before []approval, apps []move) {
	if len(before) == 0 || len(before) != len(apps) {
		return
	}
	for _, a := range before {
		if !a.approved && !a.unquarantined {
			return
		}
	}
	for i, a := range before {
		if a.identity == "" {
			in.warnf("brw couldn't verify %s's signer so macOS may prompt at next launch.", token)
			return
		}
		match, ok := in.identityMatch(ctx, apps[i].target, a.identity)
		if !ok {
			in.warnf("brw couldn't verify %s's signer so macOS may prompt at next launch.", token)
			return
		}
		if !match {
			in.warnf("%s's signer changed so macOS may prompt at next launch.", token)
			return
		}
	}
	for _, m := range apps {
		if err := inheritApproval(ctx, m.target); err != nil {
			in.warnf("brw couldn't inherit %s's quarantine approval so macOS may prompt at next launch.", token)
		}
	}
}

func inheritApproval(ctx context.Context, app string) error {
	value := readQuarantine(ctx, app)
	if value == "" {
		return nil
	}
	if flags, rest, ok := strings.Cut(value, ";"); ok {
		if n, err := strconv.ParseUint(flags, 16, 32); err == nil {
			value = fmt.Sprintf("%0*x;%s", len(flags), n|userApproved, rest)
		}
	}
	var paths bytes.Buffer
	err := filepath.WalkDir(app, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink != 0 {
			return err
		}
		paths.WriteString(p)
		paths.WriteByte(0)
		return nil
	})
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/xargs", "-0", "--", xattrTool, "-w", quarantineAttr, value)
	cmd.Stdin = &paths
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("inherit approval for %s: %w: %s", app, err, bytes.TrimSpace(out))
	}
	return nil
}
