package cask

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jigsmiths/brw/internal/poststep"
)

func readSteps(args json.RawMessage) ([]json.RawMessage, error) {
	var blocks []struct {
		Steps []json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal(args, &blocks); err != nil {
		return nil, err
	}
	var out []json.RawMessage
	for _, b := range blocks {
		out = append(out, b.Steps...)
	}
	return out, nil
}

func unsupportedSteps(steps []json.RawMessage) string {
	return strings.Join(poststep.Unsupported(steps), ", ")
}

func (in *Installer) stepContext(token, version, staged string) poststep.Context {
	arch := "intel"
	if in.Host.Arch == "arm64" {
		arch = "arm"
	}
	return poststep.Context{
		Root: in.Root, Prefix: in.Where.Prefix, Name: token, Version: version, OS: in.Host.OS,
		User: os.Getenv("USER"), Log: in.Log, Token: token, Staged: staged, Arch: arch, AppDir: in.Where.AppDir,
	}
}

func (in *Installer) runSteps(kind, token, version, staged string, steps []json.RawMessage) error {
	if len(steps) == 0 {
		return nil
	}
	if err := poststep.Run(in.stepContext(token, version, staged), steps); err != nil {
		return fmt.Errorf("%s: %s: %w", token, kind, err)
	}
	return nil
}
