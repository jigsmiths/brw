package guard

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCommitMsgHook(t *testing.T) {
	hook, err := filepath.Abs(filepath.Join("..", "..", ".githooks", "commit-msg"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		msg  string
		ok   bool
	}{
		{"subject only", "feat: add thing\n", true},
		{"scope", "fix(api): handle 304\n", true},
		{"breaking", "feat!: drop old flag\n", true},
		{"bullets", "feat: add thing\n\n- first\n- second\n- third\n", true},
		{"comment lines ignored", "feat: add thing\n# Please enter the commit message\n", true},
		{"fixup", "fixup! feat: add thing\n", true},
		{"amend", "amend! feat: add thing\n\nfeat: new subject\n", true},
		{"merge", "Merge branch 'x' into main\n", true},

		{"empty", "\n", false},
		{"no type", "add thing\n", false},
		{"unknown type", "wip: add thing\n", false},
		{"capitalised type", "Feat: add thing\n", false},
		{"no space after colon", "feat:add thing\n", false},
		{"long subject", "feat: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", false},
		{"four bullets", "feat: add thing\n\n- a\n- b\n- c\n- d\n", false},
		{"prose body", "feat: add thing\n\nThis adds a thing.\n", false},
		{"no blank line", "feat: add thing\n- a\n", false},
		{"em dash in subject", "feat: add thing \u2014 quickly\n", false},
		{"em dash in bullet", "feat: add thing\n\n- first \u2014 second\n", false},
		{"co-authored-by", "feat: add thing\n\n- a\n\nCo-Authored-By: Someone <a@b.c>\n", false},
		{"co-authored-by lowercase", "feat: add thing\n\nco-authored-by: Someone <a@b.c>\n", false},
		{"session link", "feat: add thing\n\nClaude-Session: https://example.com/x\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
			if err := os.WriteFile(file, []byte(tt.msg), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("bash", hook, file).CombinedOutput()
			if tt.ok && err != nil {
				t.Errorf("rejected a valid message: %v\n%s", err, out)
			}
			var exit *exec.ExitError
			if !tt.ok && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
				t.Errorf("want rejection (exit 1) for %q, got %v\n%s", tt.msg, err, out)
			}
		})
	}
}
