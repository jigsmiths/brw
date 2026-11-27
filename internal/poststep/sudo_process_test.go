package poststep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/sudo"
)

func (f *sudoFixture) pauses(t *testing.T) *[]time.Duration {
	t.Helper()
	var got []time.Duration
	old := pause
	pause = func(_ context.Context, d time.Duration) { got = append(got, d) }
	t.Cleanup(func() { pause = old })
	return &got
}

func TestRequiresSudoCountsOnlyMandatoryElevation(t *testing.T) {
	for step, want := range map[string]bool{
		`{"type":"run","sudo":true}`:                  true,
		`{"type":"remove","sudo":true}`:               true,
		`{"type":"terminate_process","sudo":true}`:    true,
		`{"type":"delete_keychain_certificate"}`:      true,
		`{"type":"remove","sudo":"if_needed"}`:        false,
		`{"type":"symlink","sudo":"if_needed"}`:       false,
		`{"type":"terminate_process","sudo":false}`:   false,
		`{"type":"terminate_process"}`:                false,
		`{"type":"run","command":{"path":"/bin/ls"}}`: false,
	} {
		if got := RequiresSudo([]json.RawMessage{json.RawMessage(step)}); got != want {
			t.Errorf("RequiresSudo(%s) = %t, want %t", step, got, want)
		}
	}
}

func TestTerminateProcessKillsByNameThroughSudo(t *testing.T) {
	f := newSudoFixture(t)
	f.mustRun(`[{"type":"terminate_process","name":"Docker {{version}}","sudo":true}]`)
	c := f.only()
	if c.Exe != "/usr/bin/killall" || !slices.Equal(c.Args, []string{"Docker 1.2.3"}) || c.Privilege != sudo.Sudo || !c.MustSucceed {
		t.Errorf("runner got %v", c)
	}
	if c.Stdout != io.Writer(&f.log) {
		t.Errorf("stdout = %v, brew prints the command's stdout", c.Stdout)
	}
}

func TestTerminateProcessMatchesTheFullCommandLine(t *testing.T) {
	f := newSudoFixture(t)
	f.mustRun(`[{"type":"terminate_process","name":"Docker.*daemon","match":"full"}]`)
	c := f.only()
	if c.Exe != "/usr/bin/pkill" || !slices.Equal(c.Args, []string{"-f", "Docker.*daemon"}) || c.Privilege != sudo.User {
		t.Errorf("runner got %v, want pkill -f as the user", c)
	}
}

func TestTerminateProcessAnnouncesNotices(t *testing.T) {
	f := newSudoFixture(t)
	f.mustRun(`[{"type":"terminate_process","name":"x","notices":["Stopping {{name}}","Wait"]}]`)
	if got, want := f.log.String(), "==> Stopping pkg\n==> Wait\n"; got != want {
		t.Errorf("log = %q, want %q", got, want)
	}
}

func TestTerminateProcessRetriesThenWarnsAndStopsQuietly(t *testing.T) {
	f := newSudoFixture(t)
	slept := f.pauses(t)
	f.stub.err = commandFailure(t)
	f.mustRun(`[{"type":"terminate_process","name":"x","attempts":3,"failure_message":"Could not stop {{name}}"}]`)
	if len(f.stub.cmds) != 3 || !slices.Equal(*slept, []time.Duration{time.Second, time.Second}) {
		t.Errorf("commands = %d, pauses = %v, want 3 attempts and 2 pauses of a second", len(f.stub.cmds), *slept)
	}
	if got, want := f.log.String(), "Warning: Could not stop pkg\n"; got != want {
		t.Errorf("log = %q, want %q", got, want)
	}
}

func TestTerminateProcessMustSucceedFailsAfterTheLastAttempt(t *testing.T) {
	f := newSudoFixture(t)
	f.pauses(t)
	f.stub.err = commandFailure(t)
	if err := f.run(`[{"type":"terminate_process","name":"x","must_succeed":true}]`); !errors.Is(err, f.stub.err) {
		t.Errorf("err = %v, want the command failure", err)
	}
	if len(f.stub.cmds) != 1 {
		t.Errorf("commands = %d, want the default single attempt", len(f.stub.cmds))
	}
}

func TestTerminateProcessStopsOnceTheCommandSucceeds(t *testing.T) {
	f := newSudoFixture(t)
	f.pauses(t)
	failure := commandFailure(t)
	calls := 0
	f.ctx.Sudo = func(_ context.Context, c sudo.Command) (sudo.Result, error) {
		calls++
		if calls == 1 {
			return sudo.Result{}, failure
		}
		return sudo.Result{}, nil
	}
	f.mustRun(`[{"type":"terminate_process","name":"x","attempts":5,"must_succeed":true}]`)
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestTerminateProcessReturnsOtherErrorsAtOnce(t *testing.T) {
	f := newSudoFixture(t)
	f.pauses(t)
	f.stub.err = errors.New("cannot start")
	if err := f.run(`[{"type":"terminate_process","name":"x","attempts":3}]`); !errors.Is(err, f.stub.err) {
		t.Errorf("err = %v", err)
	}
	if len(f.stub.cmds) != 1 {
		t.Errorf("commands = %d, want no retry of an error that is not a command failure", len(f.stub.cmds))
	}
}

func (f *sudoFixture) keychain(found string) {
	f.ctx.Sudo = func(_ context.Context, c sudo.Command) (sudo.Result, error) {
		f.stub.cmds = append(f.stub.cmds, c)
		switch {
		case c.Exe == "/usr/bin/openssl":
			return sudo.Result{Stdout: "sha256 Fingerprint=aa:bb:0C\nignored\n"}, nil
		case c.Exe == "/usr/bin/security" && c.Args[0] == "find-certificate":
			return sudo.Result{Stdout: found}, nil
		}
		return sudo.Result{}, nil
	}
}

func (f *sudoFixture) wantCalls(want ...string) {
	f.t.Helper()
	var got []string
	for _, c := range f.stub.cmds {
		got = append(got, fmt.Sprintf("%d %s %s", c.Privilege, c.Exe, strings.Join(c.Args, " ")))
	}
	if !slices.Equal(got, want) {
		f.t.Errorf("commands = %q, want %q", got, want)
	}
}

func TestDeleteKeychainCertificateDeletesEveryMatchingCertificate(t *testing.T) {
	f := newSudoFixture(t)
	f.keychain("SHA-256 hash: AB12\nkeychain: x\nSHA-256 hash:   cd34 \n")
	f.mustRun(`[{"type":"delete_keychain_certificate","name":"Docker Root"}]`)
	s := fmt.Sprint(sudo.Sudo)
	f.wantCalls(
		s+" /usr/bin/security find-certificate -a -c Docker Root -Z",
		s+" /usr/bin/security delete-certificate -Z AB12",
		s+" /usr/bin/security delete-certificate -Z CD34",
	)
	if c := f.stub.cmds[1]; c.Stdout != io.Writer(&f.log) || !c.MustSucceed {
		t.Errorf("delete = %v, want printed stdout and must succeed", c)
	}
}

func TestDeleteKeychainCertificateMatchesTheFingerprintOfAFile(t *testing.T) {
	f := newSudoFixture(t)
	cert := filepath.Join(f.dir, "ca.pem")
	if err := os.WriteFile(cert, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.keychain("SHA-256 hash: aabb0c\nSHA-256 hash: FFFF\n")
	f.mustRun(fmt.Sprintf(`[{"type":"delete_keychain_certificate","name":"CA","matching_certificate":{"path":%q}}]`, cert))
	u, s := fmt.Sprint(sudo.User), fmt.Sprint(sudo.Sudo)
	f.wantCalls(
		u+" /usr/bin/openssl x509 -fingerprint -sha256 -noout -in "+cert,
		s+" /usr/bin/security find-certificate -a -c CA -Z",
		s+" /usr/bin/security delete-certificate -Z AABB0C",
	)
}

func TestDeleteKeychainCertificateSkipsAMissingFileAndAnAbsentFingerprint(t *testing.T) {
	f := newSudoFixture(t)
	f.keychain("SHA-256 hash: FFFF\n")
	missing := filepath.Join(f.dir, "missing.pem")
	f.mustRun(fmt.Sprintf(`[{"type":"delete_keychain_certificate","name":"CA","matching_certificate":{"path":%q}}]`, missing))
	if len(f.stub.cmds) != 0 {
		t.Errorf("commands = %v, want none for a missing file", f.stub.cmds)
	}
	cert := filepath.Join(f.dir, "ca.pem")
	if err := os.WriteFile(cert, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.mustRun(fmt.Sprintf(`[{"type":"delete_keychain_certificate","name":"CA","matching_certificate":{"path":%q}}]`, cert))
	for _, c := range f.stub.cmds {
		if len(c.Args) > 0 && c.Args[0] == "delete-certificate" {
			t.Errorf("deleted %v although the keychain has no such fingerprint", c.Args)
		}
	}
}
