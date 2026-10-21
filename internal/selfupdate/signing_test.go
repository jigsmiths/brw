package selfupdate

import (
	"encoding/hex"
	"encoding/json"
	"sync"
	"testing"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/tlog"
	"google.golang.org/protobuf/encoding/protojson"
)

// sigstore is a Sigstore of its own for tests: it can sign what the real one signs.
type sigstore = ca.VirtualSigstore

var trusted = sync.OnceValue(func() *sigstore { return newSigstore() })

func newSigstore() *sigstore {
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		panic(err)
	}
	return vs
}

// sign returns a bundle, as cosign writes it, of identity signing artifact.
func sign(t *testing.T, vs *sigstore, identity, issuer string, artifact []byte) []byte {
	t.Helper()
	entity, err := vs.SignWithVersion(identity, issuer, artifact, "v0.1")
	if err != nil {
		t.Fatal(err)
	}
	content, err := entity.SignatureContent()
	if err != nil {
		t.Fatal(err)
	}
	msg := content.MessageSignatureContent()
	var chain []*protocommon.X509Certificate
	vc, err := entity.VerificationContent()
	if err != nil {
		t.Fatal(err)
	}
	chain = append(chain, &protocommon.X509Certificate{RawBytes: vc.Certificate().Raw})
	for _, c := range vc.Intermediates() {
		chain = append(chain, &protocommon.X509Certificate{RawBytes: c.Raw})
	}
	entries, err := entity.TlogEntries()
	if err != nil {
		t.Fatal(err)
	}
	pb := &protobundle.Bundle{
		MediaType: "application/vnd.dev.sigstore.bundle+json;version=0.1",
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_X509CertificateChain{
				X509CertificateChain: &protocommon.X509CertificateChain{Certificates: chain},
			},
		},
		Content: &protobundle.Bundle_MessageSignature{MessageSignature: &protocommon.MessageSignature{
			MessageDigest: &protocommon.HashOutput{Algorithm: protocommon.HashAlgorithm_SHA2_256, Digest: msg.Digest()},
			Signature:     msg.Signature(),
		}},
	}
	for _, e := range entries {
		tle := e.TransparencyLogEntry()
		set, err := vs.RekorSignPayload(tlog.RekorPayload{
			Body:           e.Body(),
			IntegratedTime: e.IntegratedTime().Unix(),
			LogIndex:       e.LogIndex(),
			LogID:          hex.EncodeToString([]byte(e.LogKeyID())),
		})
		if err != nil {
			t.Fatal(err)
		}
		tle.KindVersion = &protorekor.KindVersion{Kind: "hashedrekord", Version: "0.0.1"}
		tle.InclusionPromise = &protorekor.InclusionPromise{SignedEntryTimestamp: set}
		pb.VerificationMaterial.TlogEntries = append(pb.VerificationMaterial.TlogEntries, tle)
	}
	out, err := protojson.Marshal(pb)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out) {
		t.Fatal("bundle is not JSON")
	}
	return out
}
