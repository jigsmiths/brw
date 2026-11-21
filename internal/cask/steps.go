package cask

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jigsmiths/brw/internal/poststep"
)

// readSteps reads the steps of a preflight_steps or postflight_steps artifact,
// whose arguments are blocks of steps.
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
		User: os.Getenv("USER"), Log: in.Log, Token: token, Staged: staged, Arch: arch,
	}
}

// runSteps runs a cask's steps. Brew runs them in a sandbox; brw runs them as
// it runs a formula's post_install steps, with writes kept to the prefix.
func (in *Installer) runSteps(kind, token, version, staged string, steps []json.RawMessage) error {
	if len(steps) == 0 {
		return nil
	}
	if err := poststep.Run(in.stepContext(token, version, staged), steps); err != nil {
		return fmt.Errorf("%s: %s: %w", token, kind, err)
	}
	return nil
}
