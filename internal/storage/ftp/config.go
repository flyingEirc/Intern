// Package ftp stores immutable objects on an explicitly selected plain FTP
// server. Credentials are sent in cleartext: use a dedicated account, a trusted
// network and server-side confinement. Reported symlinks are refused, but FTP
// cannot prove physical filesystem confinement against a dishonest server.
package ftp

import (
	"crypto/rand"
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
// Only TLSMode "plain" is implemented; there is no implicit downgrade.
// Zero timeouts select 10-second dial and 30-second rolling idle deadlines.
type Config struct {
	Address     string
	Username    string
	Password    string
	BasePath    string
	InstanceID  string
	TLSMode     string
	DialTimeout time.Duration
	IdleTimeout time.Duration
}

func normalizeConfig(cfg Config) (Config, error) {
	if err := validateConfig(cfg); err != nil {
		return Config{}, failure("configure", err)
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 30 * time.Second
	}
	return cfg, nil
}

func validateConfig(cfg Config) error {
	if cfg.TLSMode != "plain" || cfg.DialTimeout < 0 || cfg.IdleTimeout < 0 {
		return fs.ErrInvalid
	}
	for _, value := range []string{cfg.Address, cfg.Username, cfg.Password, cfg.BasePath, cfg.InstanceID} {
		if strings.ContainsAny(value, "\x00\r\n") {
			return fs.ErrInvalid
		}
	}
	if err := validateAddress(cfg.Address, cfg.Username); err != nil {
		return err
	}
	return validateNamespace(cfg.BasePath, cfg.InstanceID)
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
	if !component(instance) || reserved(instance) {
		return fs.ErrInvalid
	}
	return nil
}

func component(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00\r\n")
}

func reserved(value string) bool {
	value = strings.ToLower(value)
	return strings.HasPrefix(value, ".intern-") || strings.HasPrefix(value, ".pending-") || strings.HasSuffix(value, ".part")
}

func (r *Repository) key(key string, root, temporary bool) error {
	if !fs.ValidPath(key) || path.Clean(key) != key || strings.ContainsAny(key, "\\\x00\r\n") {
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
