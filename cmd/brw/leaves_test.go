package main

import "testing"

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
