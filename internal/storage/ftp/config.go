// Package ftp stores immutable objects on an explicitly selected FTP or FTPS
// server. TLSMode "plain" sends credentials in cleartext: use a dedicated
// account, a trusted network and server-side confinement. TLSMode "explicit"
// (AUTH TLS) and "implicit" encrypt both channels, verify the server
// certificate against the address host and never fall back to plaintext.
// Reported symlinks are refused, but FTP cannot prove physical filesystem
// confinement against a dishonest server.
package ftp

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"io/fs"
	"net"
	"path"
	"strconv"
	"strings"
	"time"
)

const markerName = ".intern-instance"

// Config selects one instance namespace beneath an existing FTP directory.
// TLSMode is required and exact: "plain", "explicit" or "implicit". There is no
// negotiation and no implicit downgrade; anything else is refused before dial.
// CAPem is the FTPS-only trust bundle: PEM certificates for the issuing CA and
// any intermediates. Empty means the system roots. Supplying it for plain is a
// configuration error, because nothing would use it.
// Zero timeouts select 10-second dial and 30-second rolling idle deadlines.
// Path components may contain internal spaces and glob characters literally,
// but not C0/DEL control bytes or leading/trailing ASCII spaces.
type Config struct {
	Address     string
	Username    string
	Password    string
	BasePath    string
	InstanceID  string
	TLSMode     string
	CAPem       string
	DialTimeout time.Duration
	IdleTimeout time.Duration
}

// normalizeConfig validates the selection and builds the transport settings a
// Repository keeps for its whole life. The returned tls.Config is nil for plain.
func normalizeConfig(cfg Config) (Config, *tls.Config, error) {
	if err := validateConfig(cfg); err != nil {
		return Config{}, nil, failure("configure", err)
	}
	roots, err := rootPool(cfg.CAPem)
	if err != nil {
		return Config{}, nil, failure("configure", err)
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 30 * time.Second
	}
	return cfg, buildTLS(cfg, addressHost(cfg.Address), roots), nil
}

func validateConfig(cfg Config) error {
	if err := validateTransport(cfg); err != nil {
		return err
	}
	for _, value := range []string{cfg.Address, cfg.Username, cfg.Password} {
		if !protocolText(value) {
			return fs.ErrInvalid
		}
	}
	if err := validateAddress(cfg.Address, cfg.Username); err != nil {
		return err
	}
	if cfg.TLSMode != TLSModePlain && addressHost(cfg.Address) == "" {
		return fs.ErrInvalid
	}
	return validateNamespace(cfg.BasePath, cfg.InstanceID)
}

// validateTransport judges the selection itself: one of the three modes, a trust
// bundle only where a bundle is used, a bundle the transport will accept at all,
// and timeouts that can be turned into deadlines. A host name an FTPS selection
// cannot derive a server identity from is judged with the address, in
// validateConfig, because that is where the address is parsed.
func validateTransport(cfg Config) error {
	switch cfg.TLSMode {
	case TLSModePlain, TLSModeExplicit, TLSModeImplicit:
	default:
		return fs.ErrInvalid
	}
	if len(cfg.CAPem) > maxCAPemBytes || cfg.DialTimeout < 0 || cfg.IdleTimeout < 0 {
		return fs.ErrInvalid
	}
	if cfg.TLSMode == TLSModePlain && cfg.CAPem != "" {
		return fs.ErrInvalid
	}
	return nil
}

func validateAddress(address, username string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || username == "" {
		return fs.ErrInvalid
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return fs.ErrInvalid
	}
	return nil
}

func validateNamespace(base, instance string) error {
	if !path.IsAbs(base) || path.Clean(base) != base || strings.Contains(base, "\\") {
		return fs.ErrInvalid
	}
	if base != "/" {
		for part := range strings.SplitSeq(strings.TrimPrefix(base, "/"), "/") {
			if !component(part) {
				return fs.ErrInvalid
			}
		}
	}
	if !component(instance) || reserved(instance) {
		return fs.ErrInvalid
	}
	return nil
}

func component(value string) bool {
	return value != "" && value != "." && value != ".." &&
		value[0] != ' ' && value[len(value)-1] != ' ' &&
		!strings.ContainsAny(value, "/\\") && protocolText(value)
}

// Refuse command bytes that FTP servers may rewrite into another name.
func protocolText(value string) bool {
	for index := range len(value) {
		if value[index] < 0x20 || value[index] == 0x7f {
			return false
		}
	}
	return true
}

func reserved(value string) bool {
	value = strings.ToLower(value)
	return strings.HasPrefix(value, ".intern-") || strings.HasPrefix(value, ".pending-") || strings.HasSuffix(value, ".part")
}

func (r *Repository) key(key string, root, temporary bool) error {
	if !fs.ValidPath(key) || path.Clean(key) != key || strings.Contains(key, "\\") {
		return fs.ErrInvalid
	}
	parts := strings.Split(key, "/")
	if parts[0] != r.cfg.InstanceID {
		return ErrOwnership
	}
	if len(parts) == 1 && !root {
		return fs.ErrInvalid
	}
	return keyComponents(parts, temporary)
}

func keyComponents(parts []string, temporary bool) error {
	for i, part := range parts {
		if !component(part) {
			return fs.ErrInvalid
		}
		if temporary && i == len(parts)-1 {
			if !partName(part) {
				return fs.ErrInvalid
			}
		} else if reserved(part) {
			return fs.ErrInvalid
		}
	}
	return nil
}

func partName(name string) bool {
	const prefix = ".intern-upload-"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".part") {
		return false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".part")
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func temporaryKey(dir string) string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic("cryptographic random source failed")
	}
	return path.Join(dir, ".intern-upload-"+hex.EncodeToString(id[:])+".part")
}

func (r *Repository) absolute(key string) string {
	return path.Join(r.cfg.BasePath, key)
}
