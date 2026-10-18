package cmd

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/flyingEirc/Intern/internal/fsutil"
)

type configuration struct {
	Listen          string        `mapstructure:"listen"`
	MetadataDir     string        `mapstructure:"metadata_dir"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
	TrustedProxies  []string      `mapstructure:"trusted_proxies"`
	Log             struct {
		Level string `mapstructure:"level"`
	} `mapstructure:"log"`
	TLS struct {
		CertFile string `mapstructure:"cert_file"`
		KeyFile  string `mapstructure:"key_file"`
	} `mapstructure:"tls"`
}

func (l *configLoader) load() (configuration, error) {
	var c configuration
	if l.initErr != nil {
		return c, l.initErr
	}
	path, err := l.flags.GetString("config")
	if err != nil {
		return c, err
	}
	if !l.flags.Changed("config") {
		path = os.Getenv("INTERN_CONFIG")
	}
	if path != "" {
		if err = l.readConfig(path); err != nil {
			return c, err
		}
	}
	if err = l.v.UnmarshalExact(&c); err != nil {
		return c, errors.New("invalid configuration key or value type")
	}
	return c, c.validate()
}

func (l *configLoader) readConfig(path string) (retErr error) {
	// openConfig uses O_NONBLOCK on Unix so a FIFO with no writer cannot block in open(2).
	f, err := openConfig(path)
	if err != nil {
		return fmt.Errorf("cannot open YAML configuration %q: %w", path, err)
	}
	defer fsutil.Close(&retErr, f)
	// Stat the opened descriptor before reading. Viper drops reader errors, so a
	// non-regular file would otherwise be treated as an empty configuration.
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("cannot read YAML configuration %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cannot read YAML configuration %q: not a regular file", path)
	}
	if err = setConfigBlocking(f); err != nil {
		return fmt.Errorf("cannot read YAML configuration %q: %w", path, err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("cannot read YAML configuration %q: %w", path, err)
	}
	l.v.SetConfigType("yaml")
	if err = l.v.ReadConfig(bytes.NewReader(data)); err != nil {
		return fmt.Errorf("cannot read YAML configuration %q (unreadable or invalid syntax): %w", path, err)
	}
	return nil
}

func (c configuration) validate() error {
	if err := validateListen(c.Listen); err != nil {
		return err
	}
	if c.MetadataDir == "" || !filepath.IsAbs(c.MetadataDir) {
		return errors.New("metadata_dir must be a non-empty absolute path")
	}
	if _, err := c.logLevel(); err != nil {
		return err
	}
	if c.ShutdownTimeout <= 0 {
		return errors.New("shutdown_timeout must be positive")
	}
	if err := validateProxies(c.TrustedProxies); err != nil {
		return err
	}
	return c.validateTLS()
}

func validateListen(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", address, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return errors.New("invalid listen address: port must be an integer between 0 and 65535")
	}
	if host == "" {
		return nil
	}
	if _, err = netip.ParseAddr(host); err == nil {
		return nil
	}
	if !validHostname(host) {
		return errors.New("invalid listen address: host must be an IP address or DNS hostname")
	}
	return nil
}

func validHostname(host string) bool {
	if len(host) > 254 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, c := range label {
			if !validHostnameChar(c) {
				return false
			}
		}
	}
	return true
}

func validHostnameChar(c rune) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-'
}

func validateProxies(proxies []string) error {
	for _, proxy := range proxies {
		if err := validateProxy(proxy); err != nil {
			return fmt.Errorf("invalid trusted_proxies entry %q: %w", proxy, err)
		}
	}
	return nil
}

// validateProxy applies the acceptance rules of gin.Engine.SetTrustedProxies.
// netip accepts zone-scoped IPv6, which Gin rejects before it can trust the proxy.
func validateProxy(proxy string) error {
	candidate := proxy
	if !strings.Contains(proxy, "/") {
		ip := net.ParseIP(proxy)
		if ip == nil {
			return &net.ParseError{Type: "IP address", Text: proxy}
		}
		if ipv4 := ip.To4(); ipv4 != nil {
			ip = ipv4
		}
		switch len(ip) {
		case net.IPv4len:
			candidate += "/32"
		case net.IPv6len:
			candidate += "/128"
		default:
			return &net.ParseError{Type: "IP address", Text: proxy}
		}
	}
	if _, _, err := net.ParseCIDR(candidate); err != nil {
		return err
	}
	return nil
}

func (c configuration) validateTLS() error {
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return errors.New("tls.cert_file and tls.key_file must be supplied together")
	}
	if c.TLS.CertFile == "" {
		return nil
	}
	if _, err := tls.LoadX509KeyPair(c.TLS.CertFile, c.TLS.KeyFile); err != nil {
		return fmt.Errorf("cannot load TLS certificate and private key: %w", err)
	}
	return nil
}

func (c configuration) logLevel() (slog.Level, error) {
	switch c.Log.Level {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, errors.New("log.level must be debug, info, warn, or error")
	}
}
