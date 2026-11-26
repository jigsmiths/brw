package poststep

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/jigsmiths/brw/internal/fsx"
)

const (
	childUsage       = "usage: %s PAYLOAD SHA256"
	childFailureExit = 1
	parentGreeting   = "brw-sandbox-parent"
	handshakeTimeout = 10 * time.Second
)

// parentChannel is how the child asks its parent to run a privileged step.
type parentChannel interface {
	request(index int) error
}

type noChannel struct{}

func (noChannel) request(index int) error {
	return fmt.Errorf("Privileged cask install step %d requires a parent message channel.", index) //nolint:staticcheck // brew's message, shown to the user
}

type childChannel struct {
	conn net.Conn
	in   *bufio.Reader
}

func (c *childChannel) request(index int) error {
	body, err := json.Marshal(map[string]any{"type": privilegedRequest, "index": index})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.conn, "%s\n", body); err != nil {
		return err
	}
	answer, _ := c.in.ReadString('\n')
	if strings.TrimSuffix(answer, "\n") == privilegedSucceed {
		return nil
	}
	return fmt.Errorf("The parent process failed to run privileged cask install step %d.", index) //nolint:staticcheck // brew's message, shown to the user
}

// Action is work other than a step list that the sandboxed child can be asked to do.
type Action func(ctx context.Context, c Context, extra json.RawMessage, stdout, stderr io.Writer) error

type Actions map[string]Action

func ChildMain(args []string, stdout, stderr io.Writer, actions Actions) int {
	conn, in, err := dialParent()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "brw: %v\n", err)
		return childFailureExit
	}
	defer func() { _ = conn.Close() }()
	if err := runChild(args, conn, in, stdout, stderr, actions); err != nil {
		report, _ := json.Marshal(map[string]string{"json_class": "RuntimeError", "m": err.Error()})
		_, _ = fmt.Fprintf(conn, "%s\n", report)
		return childFailureExit
	}
	return 0
}

// dialParent connects to the parent's socket and waits for it to greet, so a child with a stale or forged variable does not run.
func dialParent() (net.Conn, *bufio.Reader, error) {
	socket := os.Getenv(errorPipeEnv)
	if socket == "" {
		return nil, nil, errors.New("this command only runs as a sandboxed step runner")
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return nil, nil, err
	}
	in := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	greeting, err := in.ReadString('\n')
	if err != nil || strings.TrimSuffix(greeting, "\n") != parentGreeting {
		_ = conn.Close()
		return nil, nil, errors.New("the parent process did not answer")
	}
	return conn, in, conn.SetReadDeadline(time.Time{})
}

func runChild(args []string, conn net.Conn, in *bufio.Reader, stdout, stderr io.Writer, actions Actions) error {
	if len(args) != 2 {
		return fmt.Errorf(childUsage, ChildCommand)
	}
	body, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != args[1] {
		return errors.New("Cask sandbox payload checksum mismatch.") //nolint:staticcheck // brew's message, shown to the user
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	action, custom := actions[p.Action]
	if !custom && p.Action != installStepsAction && p.Action != formulaAction {
		return fmt.Errorf("unknown sandboxed cask action: %s", p.Action)
	}
	root, err := fsx.Open(p.Context.Prefix, nil)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	cp := p.Context
	c := Context{
		Root: root, Prefix: cp.Prefix, Name: cp.Name, Version: cp.Version, Keg: cp.Keg, OS: cp.OS, User: cp.User,
		Log: stdout, Err: stderr, Token: cp.Token, Staged: cp.Staged, Arch: cp.Arch, AppDir: cp.AppDir, Host: cp.Host,
		Sandbox: Unsandboxed,
	}
	if custom {
		return action(context.Background(), c, p.Extra, stdout, stderr)
	}
	var channel parentChannel = noChannel{}
	if os.Getenv(messageChannelEnv) != "" {
		channel = &childChannel{conn: conn, in: in}
	}
	r := &runner{ctx: context.Background(), c: c, guards: map[string]bool{}}
	return r.plan(p.Steps, p.Phase, privilegedDelegate{enabled: true, run: channel.request})
}
