package api

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/api/apitest"
)

func TestVerifyReturnsTheSignedPayload(t *testing.T) {
	key := apitest.Shared()
	payload := []byte(`[{"name":"jq"}]`)
	got, err := verify(apitest.Listing(key, payload), &key.PublicKey)
	if err != nil || !bytes.Equal(got, payload) {
		t.Errorf("verify = %q, %v, want %q", got, err, payload)
	}
}

func TestVerifyRefuses(t *testing.T) {
	key, other := apitest.NewKey(), apitest.NewKey()
	payload := []byte(`[{"name":"jq"}]`)
	tests := []struct {
		name string
		doc  []byte
		want string
	}{
		{"signed by another key", apitest.Listing(other, payload), "signature"},
		{"unknown key id", apitest.Sign(key, apitest.Header, "other-1", payload), "key"},
		{"other algorithm", apitest.Sign(key, `{"alg":"RS256","b64":false,"crit":["b64"]}`, "homebrew-1", payload), "algorithm"},
		{"base64 payload", apitest.Sign(key, `{"alg":"PS512","crit":["b64"]}`, "homebrew-1", payload), "algorithm"},
		{"tampered payload", bytes.Replace(apitest.Listing(key, payload), []byte("jq"), []byte("jx"), 1), "signature"},
		{"no signature at all", payload, ""},
		{"not json", []byte("<html>"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := verify(tt.doc, &key.PublicKey)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("verify error = %v, want one mentioning %q", err, tt.want)
			}
		})
	}
}

func TestHomebrewKey(t *testing.T) {
	key, err := homebrewKey()
	if err != nil {
		t.Fatal(err)
	}
	if key.N.BitLen() != 4096 {
		t.Errorf("key has %d bits, want 4096", key.N.BitLen())
	}
}

func TestFormulaeRefusesAnUnsignedListing(t *testing.T) {
	key := apitest.Shared()
	ts := httptest.NewServer(&server{body: fixture(t), raw: true}) // the plain listing, not a signed one
	defer ts.Close()
	dir := t.TempDir()
	c := &Client{URL: ts.URL, CacheDir: dir, Key: &key.PublicKey}
	if _, err := c.Formulae(context.Background()); err == nil {
		t.Fatal("an unsigned listing was accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "formula.json")); err == nil {
		t.Error("an unsigned listing was cached")
	}
}

func TestFormulaeUsesTheEmbeddedKeyByDefault(t *testing.T) {
	ts := httptest.NewServer(&server{body: fixture(t)})
	defer ts.Close()
	c := &Client{URL: ts.URL, CacheDir: t.TempDir()} // no Key: Homebrew's
	if _, err := c.Formulae(context.Background()); err == nil {
		t.Fatal("a listing signed by an unknown key was accepted")
	}
}
