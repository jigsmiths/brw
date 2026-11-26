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

	"github.com/jigsmiths/brw/internal/fsx"
)

const (
	childUsage       = "usage: %s PAYLOAD SHA256"
	childFailureExit = 1
)

type childChannel struct {
	conn net.Conn
	in   *bufio.Reader
}

// request asks the parent to run step index, which needs privileges the sandbox withholds.
func (c *childChannel) request(index int) error {
	if c == nil {
		return fmt.Errorf("Privileged cask install step %d requires a parent message channel.", index) //nolint:staticcheck // brew's message, shown to the user
	}
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

// ChildMain runs a payload as the sandboxed child and returns its exit code.
func ChildMain(args []string, stdout, stderr io.Writer) int {
	conn, err := dialParent()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "brw: %v\n", err)
		return childFailureExit
	}
	defer func() { _ = conn.Close() }()
	if err := runChild(args, conn, stdout, stderr); err != nil {
		report, _ := json.Marshal(map[string]string{"json_class": "RuntimeError", "m": err.Error()})
		_, _ = fmt.Fprintf(conn, "%s\n", report)
		return childFailureExit
	}
	return 0
}

func dialParent() (net.Conn, error) {
	socket := os.Getenv(errorPipeEnv)
	if socket == "" {
		return nil, errors.New("this command only runs as a sandboxed step runner")
	}
	return net.Dial("unix", socket)
}

func runChild(args []string, conn net.Conn, stdout, stderr io.Writer) error {
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
	if p.Action != installStepsAction && p.Action != formulaAction {
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
	var channel *childChannel
	if os.Getenv(messageChannelEnv) != "" {
		channel = &childChannel{conn: conn, in: bufio.NewReader(conn)}
	}
	r := &runner{ctx: context.Background(), c: c, guards: map[string]bool{}}
	return r.plan(p.Steps, p.Phase, channel.request)
}
