package poststep

import (
	"io"
	"os"
	"path/filepath"

	"github.com/jigsmiths/brw/internal/sandbox"
	"github.com/jigsmiths/brw/internal/sudo"
)

type SandboxPolicy struct {
	Cache      string
	Logs       string
	Repository string
	Available  func() bool
	Warn       func(message string)
}

const sandboxUnavailable = "Sandbox unavailable: running cask artifact operations without sandboxing!"

var kegLinkDirectories = []string{"bin", "etc", "include", "lib", "sbin", "share", "var"}

func (p *SandboxPolicy) available() bool {
	if p.Available != nil {
		return p.Available()
	}
	return sandbox.Available()
}

func (r *runner) sandboxFor(s step, home string) (*sandbox.Sandbox, error) {
	c := r.c
	policy := c.Sandbox
	sb := &sandbox.Sandbox{}
	sb.AllowRead(c.Staged)
	temp := r.tempDir()
	sb.AllowWriteSystemTemp(temp)
	sb.AllowWritePath(policy.Cache)
	sb.DenyWriteHomebrewRepository(filepath.Join(c.Prefix, "bin/brew"), c.Prefix, policy.Repository)
	realHome, err := r.base("home", "")
	if err != nil {
		return nil, err
	}
	sb.DenyReadHome(realHome, []string{c.Prefix, policy.Repository, policy.Cache, policy.Logs, temp}, func(m string) { r.logf("Warning: %s", m) })
	if !s.NetworkAccess {
		sb.DenyAllNetwork()
	}
	sb.AllowWritePath(filepath.Join(c.Prefix, "Caskroom", c.Token))
	if c.AppDir != "" {
		sb.AllowWritePath(c.AppDir)
	}
	for _, d := range kegLinkDirectories {
		sb.AllowWritePath(filepath.Join(c.Prefix, d))
	}
	var writable []string
	if s.StdoutPath != nil {
		out, err := r.resolve(*s.StdoutPath)
		if err != nil {
			return nil, err
		}
		writable = append(writable, filepath.Dir(out))
	}
	for _, spec := range s.WritablePaths {
		p, err := r.resolve(spec)
		if err != nil {
			return nil, err
		}
		writable = append(writable, p)
	}
	for _, p := range writable {
		sb.AllowWritePath(p)
		if sandbox.Within(realHome, p) {
			sb.AllowRead(p)
		}
	}
	sb.AllowWritePath(home)
	sb.DenyWritePath(filepath.Join(c.Prefix, "var/homebrew/tmp/.cellar"))
	locks := filepath.Join(c.Prefix, "var/homebrew/locks")
	sb.DenyWritePath(locks)
	sb.DenyReadPath(locks)
	return sb, nil
}

func (r *runner) runSandboxed(s step, program string, args, env []string, dir, input string) (ran bool, err error) {
	policy := r.c.Sandbox
	if policy == nil || r.c.OS != "darwin" || s.Sudo == sudoAlways || !r.c.cask() {
		return false, nil
	}
	if !policy.available() {
		if policy.Warn != nil {
			policy.Warn(sandboxUnavailable)
		}
		return false, nil
	}
	home, err := os.MkdirTemp(r.tempDir(), "brw-cask-home-") //fsx:allow brew's scratch home for the sandboxed child
	if err != nil {
		return true, err
	}
	defer func() { _ = os.RemoveAll(home) }() //fsx:allow brew's scratch home for the sandboxed child
	sb, err := r.sandboxFor(s, home)
	if err != nil {
		return true, err
	}
	c := sudo.Command{
		Exe: sandbox.Exec, Args: append([]string{"-p", sb.Profile(), program}, args...),
		Env: append([]string{"HOME=" + home}, env...), Dir: dir, Input: input,
		Privilege: sudo.User, MustSucceed: !s.AllowFailure,
	}
	if s.PrintStdout {
		c.Stdout = r.c.Log
	}
	c.Stderr = r.stderr()
	if s.SuppressStderr {
		c.Stderr = io.Discard
	}
	res, err := r.sudoCommand(c)
	if err != nil || res.Failed {
		return true, err
	}
	return true, r.captureStdout(s, res.Stdout)
}
