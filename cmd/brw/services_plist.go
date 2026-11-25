package main

import (
	"bytes"
	"os"
	"os/exec"

	"github.com/jigsmiths/brw/internal/services"
)

const binaryPlistMagic = "bplist00"

func plutilXML(path string) ([]byte, error) {
	return exec.Command("/usr/bin/plutil", "-convert", "xml1", "-o", "-", path).Output()
}

// fileLabel is FormulaWrapper.service_file_label: binary plists go through plutil.
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
		if body, err = e.plutil(path); err != nil {
			return "", false
		}
	}
	return services.FileLabel(body)
}
