// Package s2fixture holds the material the FTPS acceptance fixture is built from:
// the certificates a real Pure-FTPd listener serves, the manifest the fixture
// publishes about itself, and the audit records its taps leave behind.
//
// It is a normal package rather than test code because three parties have to agree
// on these shapes exactly: the harness that starts the fixture, the tap that writes
// the records, and the worker that reads them back to decide what it proved. A
// certificate bundle is public data by definition - a CA certificate and the server
// certificates it issued are what a client is told - so the only private key here is
// the one inside the server PEM a listener loads in its own container.
package s2fixture

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// Leaf names are the fixture's certificate-bearing faces. The plain listener serves
// no certificate at all, and the implicit listener serves LeafGood.
const (
	LeafGood      = "good"
	LeafWrongHost = "wronghost"
	LeafExpired   = "expired"
	LeafRogue     = "rogue"
)

// Leaf is one server identity: the certificate, the key, and the public facts a
// worker cross-checks a real handshake against.
type Leaf struct {
	Name        string    `json:"name"`
	ServerPEM   string    `json:"-"`
	Certificate string    `json:"certificate"`
	Fingerprint string    `json:"fingerprint"`
	Serial      string    `json:"serial"`
	DNSNames    []string  `json:"dns_names"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	IssuedBy    string    `json:"issued_by"`
}

// Bundle is the fixture's whole public identity for one host name.
type Bundle struct {
	Host     string          `json:"host"`
	CA       string          `json:"ca"`
	CAFinger string          `json:"ca_fingerprint"`
	RogueCA  string          `json:"rogue_ca"`
	Leaves   map[string]Leaf `json:"leaves"`
}

// Certificates issues the bundle the listener set is built from, relative to now:
//
//   - LeafGood is issued by the test CA for host, and is the identity an FTPS
//     selection is expected to accept;
//   - LeafWrongHost is issued by the same CA - so it is trusted - but for another
//     name, so only the hostname check can refuse it;
//   - LeafExpired is issued by the same CA for the right name and is already past
//     its NotAfter, so only the validity window can refuse it;
//   - LeafRogue is self-signed for the right name, so the chain is the refusal
//     against the bundle a worker pins.
//
// Keys are ECDSA P-256 and every signature is SHA-256, which is what both the
// daemon's default cipher list and a TLS 1.2 or 1.3 client negotiate.
func Certificates(host string, now time.Time) (Bundle, error) {
	if host == "" {
		return Bundle{}, errors.New("a certificate host name is required")
	}
	bundle := Bundle{Host: host, Leaves: make(map[string]Leaf, 4)}
	caKey, caCert, err := authority("Intern FTPS Test CA", now)
	if err != nil {
		return Bundle{}, err
	}
	bundle.CA = caCert
	bundle.CAFinger = Fingerprint(caCert)
	rogueKey, rogueCert, err := authority("Intern FTPS Rogue CA", now)
	if err != nil {
		return Bundle{}, err
	}
	bundle.RogueCA = rogueCert
	for _, wanted := range []struct {
		name     string
		dns      string
		notAfter time.Time
		foreign  bool
	}{
		{LeafGood, host, now.Add(24 * time.Hour), false},
		{LeafWrongHost, "s2-some-other-host", now.Add(24 * time.Hour), false},
		{LeafExpired, host, now.Add(-time.Hour), false},
		{LeafRogue, host, now.Add(24 * time.Hour), true},
	} {
		signer, issuer := caKey, "test-ca"
		if wanted.foreign {
			signer, issuer = rogueKey, "self"
		}
		leaf, err := serve(wanted.name, wanted.dns, wanted.notAfter, now, signer, caCert, issuer)
		if err != nil {
			return Bundle{}, err
		}
		bundle.Leaves[wanted.name] = leaf
	}
	return bundle, nil
}

// String names a bundle for a log line without ever carrying a key.
func (b Bundle) String() string {
	return fmt.Sprintf("host=%s ca_fingerprint=%s leaves=%d", b.Host, b.CAFinger, len(b.Leaves))
}

// Fingerprint digests the DER of the first certificate in one PEM bundle. It is how
// a worker ties the certificate a real handshake presented back to the identity the
// fixture published in its manifest.
func Fingerprint(bundle string) string {
	block, _ := pem.Decode([]byte(bundle))
	if block == nil {
		return ""
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

func authority(common string, now time.Time) (*ecdsa.PrivateKey, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, "", err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: common, Organization: []string{"Intern FTPS fixture"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, "", err
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), nil
}

func serve(name, dnsName string, notAfter, now time.Time, signer *ecdsa.PrivateKey, caPEM, issuer string) (Leaf, error) {
	block, _ := pem.Decode([]byte(caPEM))
	if block == nil {
		return Leaf{}, errors.New("the issuing certificate is not PEM")
	}
	parent, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return Leaf{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Leaf{}, err
	}
	serial, err := serialNumber()
	if err != nil {
		return Leaf{}, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: dnsName, Organization: []string{"Intern FTPS fixture"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{dnsName},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, signer)
	if err != nil {
		return Leaf{}, err
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	marshalled, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return Leaf{}, err
	}
	private := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: marshalled})
	// Pure-FTPd loads a single file holding the chain and the key together, which is
	// what --certfile documents.
	return Leaf{
		Name:        name,
		ServerPEM:   string(certificate) + string(private),
		Certificate: string(certificate),
		Fingerprint: Fingerprint(string(certificate)),
		Serial:      serial.String(),
		DNSNames:    template.DNSNames,
		NotBefore:   template.NotBefore,
		NotAfter:    template.NotAfter,
		IssuedBy:    issuer,
	}, nil
}

func serialNumber() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}
