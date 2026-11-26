package cask

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jigsmiths/brw/internal/poststep"
	"github.com/jigsmiths/brw/internal/sudo"
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
	repository := filepath.Join(in.Where.Prefix, "Homebrew")
	if in.Where.Prefix == "/opt/homebrew" {
		repository = in.Where.Prefix
	}
	policy := &poststep.SandboxPolicy{
		Cache: in.Fetcher.Dir, Logs: filepath.Join(in.Where.Home, "Library/Logs/Homebrew"), Repository: repository,
		Warn: func(m string) {
			if !in.warnedSandbox {
				in.warnf("%s", m)
				in.warnedSandbox = true
			}
		},
	}
	if in.noSandbox {
		policy = nil
	}
	return poststep.Context{
		Sandbox: policy,
		Root:    in.Root, Prefix: in.Where.Prefix, Name: token, Version: version, OS: in.Host.OS,
		User: os.Getenv("USER"), Log: in.Log, Token: token, Staged: staged, Arch: arch, AppDir: in.Where.AppDir,
		Home: in.Where.Home, Err: in.Warn, Sudo: in.stepRunner(), Remove: in.gainPermissionsRemove,
	}
}

func (in *Installer) stepRunner() sudo.Runner {
	return func(ctx context.Context, c sudo.Command) (sudo.Result, error) {
		cmd := in.command(c.Privilege, c.Exe, c.Args...)
		cmd.MustSucceed = c.MustSucceed
		cmd.Env, cmd.Input, cmd.Dir, cmd.Stdout = c.Env, c.Input, c.Dir, c.Stdout
		if c.Stderr != nil {
			cmd.Stderr = c.Stderr
		}
		return in.runner()(ctx, cmd)
	}
}

func (in *Installer) runSteps(ctx context.Context, kind, token, version, staged string, steps []json.RawMessage) error {
	if len(steps) == 0 {
		return nil
	}
	if err := poststep.Run(ctx, in.stepContext(token, version, staged), steps); err != nil {
		return fmt.Errorf("%s: %s: %w", token, kind, err)
	}
	return nil
}
