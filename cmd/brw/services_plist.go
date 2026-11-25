package main

import (
	"bytes"
	"os"
	"os/exec"

	"github.com/jigsmiths/brw/internal/services"
)

const (
	binaryPlistMagic = "bplist00"
	plutilPath       = "/usr/bin/plutil"
)

func plutilXML(path string) ([]byte, error) {
	return exec.Command(plutilPath, "-convert", "xml1", "-o", "-", path).Output()
}

func (e *env) fileLabel(path string) (string, bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	if bytes.HasPrefix(body, []byte(binaryPlistMagic)) {
		if e.plutil == nil {
			return "", false
		}
		e.echoRun(plutilPath, "-convert", "xml1", "-o", "-", path)
		if body, err = e.plutil(path); err != nil {
			return "", false
		}
	}
	return services.FileLabel(body)
}
