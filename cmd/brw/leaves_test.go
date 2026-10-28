package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLeaves(t *testing.T) {
	prefix := prefixWith(t, map[string]string{"jq@1.8.2": "jq", "age@1.0": "age", "bash@5.3": "bash", "brotli@1.2": "brotli"})
	tests := []struct {
		args []string
		want string
	}{
		{nil, "age\nbash\nbrotli\njq\n"},
		{[]string{"--installed-on-request"}, "age\njq\n"},
		{[]string{"-r"}, "age\njq\n"},
		{[]string{"--installed-as-dependency"}, "bash\nbrotli\n"},
		{[]string{"-p"}, "bash\nbrotli\n"},
	}
	for _, tt := range tests {
		out, err := runCmd(t, &env{host: mac}, "leaves", append([]string{"-prefix", prefix}, tt.args...)...)
		if err != nil || out != tt.want {
			t.Errorf("leaves %v = %q, %v, want %q", tt.args, out, err, tt.want)
		}
	}
}

func TestLeavesRefusesBadArguments(t *testing.T) {
	prefix := prefixWith(t, nil)
	for _, args := range [][]string{{"-r", "-p"}, {"jq"}} {
		if _, err := runCmd(t, &env{host: mac}, "leaves", append([]string{"-prefix", prefix}, args...)...); err == nil {
			t.Errorf("leaves %v should fail", args)
		}
	}
}

func TestCommandsWarnAboutAnUnreadableReceiptAndCarryOn(t *testing.T) {
	prefix := prefixWith(t, map[string]string{"jq@1.8.2": "jq", "age@1.0": "age"})
	if err := os.WriteFile(filepath.Join(prefix, "Cellar/age/1.0/INSTALL_RECEIPT.json"), []byte("{oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"leaves", "outdated"} {
		e := &env{host: mac}
		if _, err := runCmd(t, e, cmd, "-prefix", prefix); err != nil && strings.Contains(err.Error(), "age 1.0") {
			t.Errorf("%s failed on the bad receipt: %v", cmd, err)
		}
		if w := e.err.(*bytes.Buffer).String(); !strings.Contains(w, "brw: warning: age 1.0:") {
			t.Errorf("%s stderr = %q, want a warning about age 1.0", cmd, w)
		}
	}
}
