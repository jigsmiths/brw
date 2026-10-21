package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProbe checks a real bundle against Sigstore's public root. Temporary.
func TestProbe(t *testing.T) {
	dir := os.Getenv("BRW_PROBE_DIR")
	if dir == "" {
		t.Skip("BRW_PROBE_DIR not set")
	}
	sums, err := os.ReadFile(filepath.Join(dir, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := os.ReadFile(filepath.Join(dir, bundleName))
	if err != nil {
		t.Fatal(err)
	}
	u := &Updater{}
	if err := u.verifySigned(sums, bundle, os.Getenv("BRW_PROBE_SIGNER"), SignerIssuer); err != nil {
		t.Fatal(err)
	}
	if err := u.verifySigned(append(sums, 'x'), bundle, os.Getenv("BRW_PROBE_SIGNER"), SignerIssuer); err == nil {
		t.Fatal("accepted changed contents")
	}
	if err := u.verifySigned(sums, bundle, SignerIdentity, SignerIssuer); err == nil {
		t.Fatal("accepted the wrong signer")
	}
}
