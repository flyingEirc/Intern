package s2fixture

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"
	"time"
)

// TestCertificatesRefuseForTheirOwnReason issues the real fixture bundle and
// verifies every leaf against the pinned test CA, exactly as an FTPS client would,
// so each refusal fixture fails for the one reason it exists for and the good
// leaf is accepted. The rogue leaf must chain to the rogue CA and to nothing else.
func TestCertificatesRefuseForTheirOwnReason(t *testing.T) {
	now := time.Now()
	bundle, err := Certificates("s2-ftps", now)
	if err != nil {
		t.Fatalf("issuing the fixture bundle: %v", err)
	}
	pinned := pool(t, bundle.CA)
	rogue := pool(t, bundle.RogueCA)
	verify := func(name string, roots *x509.CertPool) error {
		leaf, ok := bundle.Leaves[name]
		if !ok {
			t.Fatalf("bundle has no %s leaf", name)
		}
		_, err := parse(t, leaf.Certificate).Verify(x509.VerifyOptions{
			DNSName: "s2-ftps", Roots: roots, CurrentTime: now,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		return err
	}
	if err := verify(LeafGood, pinned); err != nil {
		t.Fatalf("the good leaf is refused by the pinned CA: %v", err)
	}
	var hostname x509.HostnameError
	if err := verify(LeafWrongHost, pinned); !errors.As(err, &hostname) {
		t.Fatalf("the wrong-host leaf: %v, want a hostname refusal", err)
	}
	var invalid x509.CertificateInvalidError
	if err := verify(LeafExpired, pinned); !errors.As(err, &invalid) || invalid.Reason != x509.Expired {
		t.Fatalf("the expired leaf: %v, want an expiry refusal", err)
	}
	var unknown x509.UnknownAuthorityError
	if err := verify(LeafRogue, pinned); !errors.As(err, &unknown) {
		t.Fatalf("the rogue leaf against the pinned CA: %v, want an unknown-authority refusal", err)
	}
	if err := verify(LeafRogue, rogue); err != nil {
		t.Fatalf("the rogue leaf does not chain to its own CA: %v", err)
	}
}

func pool(t *testing.T, bundlePEM string) *x509.CertPool {
	t.Helper()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(bundlePEM)) {
		t.Fatal("a CA bundle did not parse")
	}
	return roots
}

func parse(t *testing.T, certificatePEM string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(certificatePEM))
	if block == nil {
		t.Fatal("a leaf is not PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parsing a leaf: %v", err)
	}
	return certificate
}
