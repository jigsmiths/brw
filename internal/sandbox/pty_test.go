package sandbox

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"syscall"
	"testing"
)

type fakeTerminal struct {
	out   io.Reader
	mu    sync.Mutex
	typed bytes.Buffer
	wrote chan struct{}
	once  sync.Once
}

func newFakeTerminal(output string) *fakeTerminal {
	return &fakeTerminal{out: thenEIO{strings.NewReader(output)}, wrote: make(chan struct{})}
}

func (f *fakeTerminal) Read(p []byte) (int, error) { return f.out.Read(p) }

func (f *fakeTerminal) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.once.Do(func() { close(f.wrote) })
	return f.typed.Write(p)
}

func (f *fakeTerminal) written() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.typed.String()
}

type thenEIO struct{ data *strings.Reader }

func (r thenEIO) Read(p []byte) (int, error) {
	n, err := r.data.Read(p)
	if errors.Is(err, io.EOF) {
		return n, syscall.EIO
	}
	return n, err
}

func TestForwardCopiesOutputUntilTheTerminalReportsEIO(t *testing.T) {
	term := newFakeTerminal("hello\n")
	var out bytes.Buffer
	<-forward(term, nil, &out, false)
	if out.String() != "hello\n" {
		t.Errorf("out = %q", out.String())
	}
}

func TestForwardSendsStdinToTheChildWhenPassedThrough(t *testing.T) {
	term := newFakeTerminal("")
	var out bytes.Buffer
	<-forward(term, strings.NewReader("typed"), &out, true)
	<-term.wrote
	if term.written() != "typed" {
		t.Errorf("the child got %q", term.written())
	}
}

func TestForwardKeepsStdinFromTheChildOtherwise(t *testing.T) {
	term := newFakeTerminal("")
	var out bytes.Buffer
	<-forward(term, strings.NewReader("typed"), &out, false)
	if term.written() != "" {
		t.Errorf("stdin reached the child: %q", term.written())
	}
}
