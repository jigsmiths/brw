package cask

import (
	"bytes"
	"context"
	"crypto/rand"
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
	// noTranslocation is the flag brew adds to what a download holds, so macOS
	// runs the app where it is instead of from a random read-only copy.
	noTranslocation = 0x0100
)

// quarantine marks a download as one from the web, in the form brew's
// downloads carry: flags, time in hex, an empty agent and a UUID. A file
// already marked keeps its value. It does nothing where xattr is missing.
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

// propagated is a download's quarantine value as brew puts it on what the
// download holds, or "" if the value cannot be read.
func propagated(value string) string {
	flags, rest, ok := strings.Cut(value, ";")
	n, err := strconv.ParseUint(flags, 16, 16)
	if !ok || err != nil {
		return ""
	}
	return fmt.Sprintf("%04x;%s", n|noTranslocation, rest)
}

// propagateQuarantine marks everything under dir, but not dir itself, with the
// download's quarantine, making each writable first as brew does.
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
