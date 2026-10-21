// Package apitest signs formula listings the way formulae.brew.sh does, for tests.
package apitest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"sync"
)

// Header is the protected header of a listing signed by Homebrew.
const Header = `{"alg":"PS512","b64":false,"crit":["b64"]}`

// NewKey makes a key to sign test listings with.
func NewKey() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
}

// Shared is one key for all the tests of a run, which saves generating many.
var Shared = sync.OnceValue(NewKey)

// Sign wraps payload in a signed document under the given protected header and key id.
func Sign(key *rsa.PrivateKey, header, kid string, payload []byte) []byte {
	protected := base64.RawURLEncoding.EncodeToString([]byte(header))
	digest := sha512.Sum512([]byte(protected + "." + string(payload)))
	sig, err := rsa.SignPSS(rand.Reader, key, crypto.SHA512, digest[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	if err != nil {
		panic(err)
	}
	doc, err := json.Marshal(map[string]any{
		"payload": string(payload),
		"signatures": []map[string]any{{
			"header":    map[string]string{"kid": kid},
			"protected": protected,
			"signature": base64.URLEncoding.EncodeToString(sig), // padded, as Homebrew has it
		}},
	})
	if err != nil {
		panic(err)
	}
	return doc
}

// Listing signs payload as Homebrew would.
func Listing(key *rsa.PrivateKey, payload []byte) []byte {
	return Sign(key, Header, "homebrew-1", payload)
}
