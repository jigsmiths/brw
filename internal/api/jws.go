package api

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// homebrewKeyID names the key Homebrew signs its listings with.
const homebrewKeyID = "homebrew-1"

// homebrewPEM is the public key from Homebrew/brew's Library/Homebrew/api.
//
//go:embed homebrew-1.pem
var homebrewPEM []byte

func homebrewKey() (*rsa.PublicKey, error) {
	block, _ := pem.Decode(homebrewPEM)
	if block == nil {
		return nil, errors.New("the embedded public key is not PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("the embedded public key is not an RSA key")
	}
	return key, nil
}

// verify checks a listing signed by Homebrew (RFC 7515 with an unencoded
// payload, RSA-PSS and SHA-512) and returns the payload.
func verify(doc []byte, key *rsa.PublicKey) ([]byte, error) {
	var signed struct {
		Payload    string `json:"payload"`
		Signatures []struct {
			Header struct {
				Kid string `json:"kid"`
			} `json:"header"`
			Protected string `json:"protected"`
			Signature string `json:"signature"`
		} `json:"signatures"`
	}
	if err := json.Unmarshal(doc, &signed); err != nil {
		return nil, fmt.Errorf("not a signed listing: %w", err)
	}
	for _, s := range signed.Signatures {
		if s.Header.Kid != homebrewKeyID {
			continue
		}
		header, err := decodeBase64URL(s.Protected)
		if err != nil {
			return nil, fmt.Errorf("invalid algorithm: %w", err)
		}
		var h struct {
			Alg string `json:"alg"`
			B64 *bool  `json:"b64"`
		}
		if err := json.Unmarshal(header, &h); err != nil || h.Alg != "PS512" || h.B64 == nil || *h.B64 {
			return nil, errors.New("invalid algorithm")
		}
		sig, err := decodeBase64URL(s.Signature)
		if err != nil {
			return nil, fmt.Errorf("signature mismatch: %w", err)
		}
		digest := sha512.Sum512([]byte(s.Protected + "." + signed.Payload))
		if err := rsa.VerifyPSS(key, crypto.SHA512, digest[:], sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}); err != nil {
			return nil, fmt.Errorf("signature mismatch: %w", err)
		}
		return []byte(signed.Payload), nil
	}
	return nil, fmt.Errorf("signature key %s not found", homebrewKeyID)
}

// decodeBase64URL reads URL-safe base64 with or without padding, as Homebrew's
// listings have it.
func decodeBase64URL(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}
