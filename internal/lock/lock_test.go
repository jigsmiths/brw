package lock

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jigsmiths/brw/internal/fsx"
)

func openRoot(t *testing.T) (*fsx.Root, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := fsx.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, dir
}

func TestMain(m *testing.M) {
	if dir := os.Getenv("LOCK_HELPER_DIR"); dir != "" {
		holdAndWait(dir, os.Getenv("LOCK_HELPER_NAME"))
		return
	}
	os.Exit(m.Run())
}

// holdAndWait is the subprocess side of the cross-process test.
func holdAndWait(dir, name string) {
	r, err := fsx.Open(dir, nil)
	if err != nil {
		os.Exit(2)
	}
	if _, err := Acquire(r, name); err != nil {
		os.Exit(3)
	}
	os.Stdout.WriteString("locked\n")
	bufio.NewReader(os.Stdin).ReadByte() // wait for the parent to close stdin
}

func TestAcquirePath(t *testing.T) {
	r, dir := openRoot(t)
	l, err := Acquire(r, "openssl@3")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if _, err := os.Stat(filepath.Join(dir, "var/homebrew/locks/openssl@3.formula.lock")); err != nil {
		t.Errorf("lock file not at brew's path: %v", err)
	}
}

func TestSecondAcquireFailsFast(t *testing.T) {
	r, _ := openRoot(t)
	l, err := Acquire(r, "jq")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(r, "jq"); !errors.Is(err, ErrInProgress) {
		t.Errorf("error = %v, want ErrInProgress", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	l2, err := Acquire(r, "jq")
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	l2.Release()
}

func TestDifferentNamesAreIndependent(t *testing.T) {
	r, _ := openRoot(t)
	a, err := Acquire(r, "jq")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := Acquire(r, "git")
	if err != nil {
		t.Fatal(err)
	}
	b.Release()
}

func TestReleaseTwice(t *testing.T) {
	r, _ := openRoot(t)
	l, err := Acquire(r, "jq")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Errorf("second Release = %v, want nil", err)
	}
}

func TestInvalidNames(t *testing.T) {
	r, _ := openRoot(t)
	for _, name := range []string{"", ".", "..", "a/b", "../x", `a\b`} {
		if _, err := Acquire(r, name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Acquire(%q) error = %v, want ErrInvalidName", name, err)
		}
	}
}

func TestOtherProcessHoldsLock(t *testing.T) {
	r, dir := openRoot(t)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "LOCK_HELPER_DIR="+dir, "LOCK_HELPER_NAME=jq")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); line != "locked\n" {
		t.Fatalf("helper said %q", line)
	}
	if _, err := Acquire(r, "jq"); !errors.Is(err, ErrInProgress) {
		t.Errorf("while other process holds it: error = %v, want ErrInProgress", err)
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	l, err := Acquire(r, "jq")
	if err != nil {
		t.Fatalf("after other process exited: %v", err)
	}
	l.Release()
}

// rubyFlock tries a non-blocking exclusive flock on path, as brew does.
// It exits 0 if it got the lock and 1 if the lock was held.
const rubyFlock = `f = File.open(ARGV[0], File::RDWR | File::CREAT | File::NOFOLLOW)
exit(f.flock(File::LOCK_EX | File::LOCK_NB) ? 0 : 1)`

func TestInteropWithRubyLocking(t *testing.T) {
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("no ruby")
	}
	r, dir := openRoot(t)
	path := filepath.Join(dir, "var/homebrew/locks/jq.formula.lock")

	l, err := Acquire(r, "jq")
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(ruby, "-e", rubyFlock, path).Run(); err == nil {
		t.Error("ruby got a lock that we hold")
	}
	l.Release()
	if err := exec.Command(ruby, "-e", rubyFlock, path).Run(); err != nil {
		t.Errorf("ruby could not lock after we released: %v", err)
	}

	hold := exec.Command(ruby, "-e", `f = File.open(ARGV[0], File::RDWR | File::CREAT)
f.flock(File::LOCK_EX | File::LOCK_NB) or exit 1
puts "locked"; $stdout.flush; $stdin.read`, path)
	stdin, _ := hold.StdinPipe()
	stdout, _ := hold.StdoutPipe()
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	defer hold.Wait()
	defer stdin.Close()
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); line != "locked\n" {
		t.Fatalf("ruby said %q", line)
	}
	if _, err := Acquire(r, "jq"); !errors.Is(err, ErrInProgress) {
		t.Errorf("while ruby holds it: error = %v, want ErrInProgress", err)
	}
}
