package ftp

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io/fs"
	"net"
	"strings"
)

// The only transport modes a Config may select. There is no negotiation and no
// fallback: a repository picks one, and a server that cannot serve it is
// refused. Plain FTP stays the explicit opt-in it always was.
const (
	TLSModePlain    = "plain"
	TLSModeExplicit = "explicit"
	TLSModeImplicit = "implicit"
)

// maxCAPemBytes bounds the operator-supplied trust bundle. It is a configuration
// limit, not a memory-tuning knob: one internal CA plus its intermediates is
// far below it, and a larger blob is a mistake or an attack. It stays well under
// the Web API's 1 MiB request-body limit, so an over-limit bundle reaches the
// handler's own ca_pem rule (422) instead of the generic 413.
const maxCAPemBytes = 1 << 18

// buildTLS assembles the single tls.Config a repository shares across every
// session. ServerName is the configured address host and is reused for data
// connections, because the data channel is opened against the passive port the
// server named, not against a name the operator chose. RootCAs is nil when no
// CA bundle was supplied, which selects the system roots. Verification is
// always on: this package has no option that could disable it.
func buildTLS(cfg Config, host string, roots *x509.CertPool) *tls.Config {
	if cfg.TLSMode == TLSModePlain {
		return nil
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: host,
		RootCAs:    roots,
		// FTPS data connections resume the session the control connection
		// established; servers that enforce RFC 4217 session reuse refuse the
		// transfer otherwise. One shared cache across sessions is correct here
		// because the identity is always this one server.
		ClientSessionCache: tls.NewLRUClientSessionCache(16),
	}
}

// rootPool parses one CA bundle. An empty bundle means system roots, so it
// returns a nil pool without an error. A bundle that carries no certificate at
// all is refused: it would silently trust nothing, which is neither of the two
// choices the operator made.
func rootPool(bundle string) (*x509.CertPool, error) {
	if bundle == "" {
		return nil, nil
	}
	if len(bundle) > maxCAPemBytes {
		return nil, fs.ErrInvalid
	}
	pool := x509.NewCertPool()
	count := 0
	for rest := []byte(bundle); len(rest) != 0; {
		block, next := pem.Decode(rest)
		if block == nil {
			if strings.TrimSpace(string(rest)) == "" {
				break
			}
			return nil, fs.ErrInvalid
		}
		rest = next
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, fs.ErrInvalid
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fs.ErrInvalid
		}
		pool.AddCert(certificate)
		count++
	}
	if count == 0 {
		return nil, fs.ErrInvalid
	}
	return pool, nil
}

// MaxCAPemBytes is the largest trust bundle one repository may carry. The
// control plane quotes it so a submitted bundle is refused where it is written,
// not where it is finally used.
const MaxCAPemBytes = maxCAPemBytes

// ValidCAPem reports whether bundle is a usable FTPS trust bundle: within the
// size limit and carrying at least one parsable certificate PEM with no headers.
// The empty bundle is valid and means the system roots. Any caller that accepts
// a ca_pem from outside uses exactly this rule, so a configuration the API
// admits is a configuration this package can dial with.
func ValidCAPem(bundle string) bool {
	if len(bundle) > maxCAPemBytes {
		return false
	}
	if bundle == "" {
		return true
	}
	_, err := rootPool(bundle)
	return err == nil
}

// addressHost is the identity every FTPS certificate is checked against.
// Configuration validation already proved the address splits into a non-empty
// host and a numeric port.
func addressHost(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
}

// tlsKind classifies a verification or negotiation refusal. Only the stable
// category is public: the underlying message, which can name a host, a subject
// or an authority, never reaches the error chain the caller sees.
func tlsKind(err error) (string, bool) {
	var (
		hostname  x509.HostnameError
		authority x509.UnknownAuthorityError
		invalid   x509.CertificateInvalidError
		violation x509.ConstraintViolationError
		roots     x509.SystemRootsError
		record    tls.RecordHeaderError
		remote    *net.OpError
	)
	switch {
	case errors.As(err, &hostname):
		return "tls_hostname", true
	case errors.As(err, &invalid):
		// Go reports a certificate that is not yet valid with the same Expired
		// reason it uses for one that is past its lifetime; either way the
		// bundle is trusted but the leaf's validity window refuses the session.
		if invalid.Reason == x509.Expired {
			return "tls_expired", true
		}
		return "tls_untrusted", true
	case errors.As(err, &authority), errors.As(err, &violation), errors.As(err, &roots):
		return "tls_untrusted", true
	// The TLS stack reports a fatal alert the peer sent as a net.OpError whose
	// only operation name is "remote error"; a record header that is not TLS is
	// what a plaintext server answers a handshake with. Both are refusals, and
	// neither is a transport hiccup a caller could retry into a downgrade.
	case errors.As(err, &record), errors.As(err, &remote) && remote.Op == "remote error":
		return "tls_negotiation", true
	case errors.Is(err, ErrTLS):
		return "tls_negotiation", true
	}
	return "", false
}
