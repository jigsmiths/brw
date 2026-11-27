package poststep

import (
	"context"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jigsmiths/brw/internal/sudo"
)

const (
	killallPath  = "/usr/bin/killall"
	pkillPath    = "/usr/bin/pkill"
	securityPath = "/usr/bin/security"
	opensslPath  = "/usr/bin/openssl"

	terminateRetryDelay = time.Second
)

var pause = func(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (r *runner) privilege(s step) sudo.Privilege {
	if s.Sudo == sudoAlways {
		return sudo.Sudo
	}
	return sudo.User
}

func (r *runner) printed(c sudo.Command) sudo.Command {
	c.MustSucceed, c.Stdout, c.Stderr = true, r.c.Log, r.stderr()
	return c
}

func (r *runner) terminateProcess(s step) error {
	for _, notice := range s.Notices {
		r.logf("==> %s", r.expand(notice))
	}
	name := r.expand(s.Name)
	exe, args := killallPath, []string{name}
	if s.Match == "full" {
		exe, args = pkillPath, []string{"-f", name}
	}
	attempts := 1
	if s.Attempts != nil {
		attempts = *s.Attempts
	}
	for {
		_, err := r.sudoCommand(r.printed(sudo.Command{Exe: exe, Args: args, Privilege: r.privilege(s)}))
		if err == nil || !sudo.IsFailure(err) {
			return err
		}
		if attempts--; attempts <= 0 {
			if s.FailureMessage != "" {
				r.logf("Warning: %s", r.expand(s.FailureMessage))
			}
			if s.MustSucceed {
				return err
			}
			return nil
		}
		pause(r.ctx, terminateRetryDelay)
		if err := r.ctx.Err(); err != nil {
			return err
		}
	}
}

var certificateHash = regexp.MustCompile(`(?m)^SHA-256 hash:\s*(\S+)`)

func (r *runner) deleteKeychainCertificate(s step) error {
	var want string
	if s.MatchingCertificate != nil {
		cert, err := r.resolve(*s.MatchingCertificate)
		if err != nil {
			return err
		}
		if _, err := os.Stat(cert); err != nil {
			return nil
		}
		res, err := r.sudoCommand(sudo.Command{
			Exe: opensslPath, Args: []string{"x509", "-fingerprint", "-sha256", "-noout", "-in", cert},
			Privilege: sudo.User, MustSucceed: true,
		})
		if err != nil {
			return err
		}
		first, _, _ := strings.Cut(res.Stdout, "\n")
		_, fingerprint, _ := strings.Cut(first, "=")
		if want = strings.ToUpper(strings.TrimSpace(strings.ReplaceAll(fingerprint, ":", ""))); want == "" {
			return nil
		}
	}
	res, err := r.sudoCommand(sudo.Command{
		Exe: securityPath, Args: []string{"find-certificate", "-a", "-c", s.Name, "-Z"},
		Privilege: sudo.Sudo, MustSucceed: true,
	})
	if err != nil {
		return err
	}
	var found []string
	for _, m := range certificateHash.FindAllStringSubmatch(res.Stdout, -1) {
		found = append(found, strings.ToUpper(m[1]))
	}
	if want != "" {
		if !slices.Contains(found, want) {
			return nil
		}
		found = []string{want}
	}
	for _, hash := range found {
		_, err := r.sudoCommand(r.printed(sudo.Command{
			Exe: securityPath, Args: []string{"delete-certificate", "-Z", hash}, Privilege: sudo.Sudo,
		}))
		if err != nil {
			return err
		}
	}
	return nil
}
