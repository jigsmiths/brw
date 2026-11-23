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

const Header = `{"alg":"PS512","b64":false,"crit":["b64"]}`

func NewKey() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
}

var Shared = sync.OnceValue(NewKey)

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

func Listing(key *rsa.PrivateKey, payload []byte) []byte {
	return Sign(key, Header, "homebrew-1", payload)
}
