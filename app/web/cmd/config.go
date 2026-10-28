package cmd

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/server"
	"github.com/flyingEirc/Intern/internal/credential"
	"github.com/flyingEirc/Intern/internal/fsutil"
)

// configuration is the Web server's complete configuration. The credential and
// preflight settings are optional: without them the server still starts, and only
// the operations that need them answer 503.
type configuration struct {
	Listen               string             `mapstructure:"listen"`
	MetadataDir          string             `mapstructure:"metadata_dir"`
	ShutdownTimeout      time.Duration      `mapstructure:"shutdown_timeout"`
	TrustedProxies       []string           `mapstructure:"trusted_proxies"`
	TLSTerminatedByProxy bool               `mapstructure:"tls_terminated_by_proxy"`
	Session              auth.SessionConfig `mapstructure:"session"`
	// KEKFile seals source and FTP repository passwords.
	KEKFile string `mapstructure:"kek_file"`
	// WorkRoot is the private scratch root the physical preflight and backups use.
	WorkRoot string `mapstructure:"work_root"`
	// XtrabackupPath is the exact XtraBackup executable the preflight probes.
	XtrabackupPath string `mapstructure:"xtrabackup_path"`
	// MinWorkBytes and MinBackupBytes are positive free-space policies.
	MinWorkBytes   uint64 `mapstructure:"min_work_bytes"`
	MinBackupBytes uint64 `mapstructure:"min_backup_bytes"`
	// Restore inputs. ExportRoot is where a completed restore publishes its
	// export; it must sit outside the metadata directory and work root.
	// XbstreamPath unpacks authenticated ciphertext, MysqldPath runs the
	// rehearsal (empty disables rehearsal only). RestoreExpansionFactor is the
	// conservative multiplied payload-space estimate (default 8), and
	// RestoreUseMemory is the xtrabackup --use-memory value in bytes (0 omits
	// the flag).
	ExportRoot             string `mapstructure:"export_root"`
	XbstreamPath           string `mapstructure:"xbstream_path"`
	MysqldPath             string `mapstructure:"mysqld_path"`
	RestoreExpansionFactor int    `mapstructure:"restore_expansion_factor"`
	RestoreUseMemory       int64  `mapstructure:"restore_use_memory"`
	Log                    struct {
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
	if err := c.Session.Validate(); err != nil {
		return err
	}
	if err := c.validateTransport(); err != nil {
		return err
	}
	if err := c.validateResources(); err != nil {
		return err
	}
	return c.validateTLS()
}

// validateResources accepts every credential, preflight and restore setting as
// absent, and requires each supplied value to be usable on its own terms.
// Cross-field rules belong to the operation that needs them, so a partial
// deployment still starts and answers 503 only for the operations it cannot run.
func (c configuration) validateResources() error {
	if err := c.validateAbsolutePaths(); err != nil {
		return err
	}
	if err := c.validateRestoreEstimate(); err != nil {
		return err
	}
	if err := c.validateExportRoot(); err != nil {
		return err
	}
	return c.validateCapacityPolicy()
}

// validateAbsolutePaths requires every configured path setting to be absolute:
// a relative one would be resolved against the listener's working directory,
// which is never the deployment's intent.
func (c configuration) validateAbsolutePaths() error {
	settings := []struct{ name, value string }{
		{"kek_file", c.KEKFile},
		{"work_root", c.WorkRoot},
		{"xtrabackup_path", c.XtrabackupPath},
		{"export_root", c.ExportRoot},
		{"xbstream_path", c.XbstreamPath},
		{"mysqld_path", c.MysqldPath},
	}
	for _, setting := range settings {
		if setting.value == "" {
			continue
		}
		if !filepath.IsAbs(setting.value) {
			return fmt.Errorf("%s must be an absolute path", setting.name)
		}
	}
	return nil
}

// validateRestoreEstimate bounds the conservative space estimate inputs.
func (c configuration) validateRestoreEstimate() error {
	if c.RestoreExpansionFactor <= 0 {
		return errors.New("restore_expansion_factor must be positive")
	}
	if c.RestoreUseMemory < 0 {
		return errors.New("restore_use_memory must not be negative")
	}
	return nil
}

// validateCapacityPolicy keeps the two free-space floors symmetric.
func (c configuration) validateCapacityPolicy() error {
	if c.MinWorkBytes == 0 && c.MinBackupBytes == 0 {
		return nil
	}
	if c.MinWorkBytes == 0 {
		return errors.New("min_work_bytes must be positive when min_backup_bytes is set")
	}
	if c.MinBackupBytes == 0 {
		return errors.New("min_backup_bytes must be positive when min_work_bytes is set")
	}
	return nil
}

// validateExportRoot enforces the frozen export-root location rules at startup:
// outside the metadata and work directories, never containing the KEK, and an
// existing path must be a real directory. A local repository root is checked by
// the restore engine when it plans, because those rows live in the database.
func (c configuration) validateExportRoot() error {
	if c.ExportRoot == "" {
		return nil
	}
	if err := c.validateExportRootPlacement(); err != nil {
		return err
	}
	return c.validateExportRootKind()
}

// validateExportRootPlacement refuses an export root that shares a volume with
// the metadata directory, the work root, or the KEK: a restore must never be
// able to overwrite or reveal any of them.
func (c configuration) validateExportRootPlacement() error {
	protected := []struct{ name, path string }{
		{"metadata_dir", c.MetadataDir},
		{"work_root", c.WorkRoot},
	}
	for _, other := range protected {
		if other.path == "" {
			continue
		}
		if pathWithin(other.path, c.ExportRoot) {
			return fmt.Errorf("export_root must be outside %s", other.name)
		}
		if pathWithin(c.ExportRoot, other.path) {
			return fmt.Errorf("%s must be outside export_root", other.name)
		}
	}
	if c.KEKFile != "" && pathWithin(c.ExportRoot, c.KEKFile) {
		return errors.New("export_root must not contain kek_file")
	}
	return nil
}

// validateExportRootKind accepts a root that does not exist yet (serve creates
// it) and rejects a symlink or a non-directory.
func (c configuration) validateExportRootKind() error {
	info, err := os.Lstat(c.ExportRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot inspect export_root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("export_root must be a non-symlink directory")
	}
	if !info.IsDir() {
		return errors.New("export_root must be a directory")
	}
	return nil
}

// pathWithin reports whether child is parent itself or sits below it.
func pathWithin(parent, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// prepareExportRoot creates the configured export root with the frozen export
// directory mode and rechecks that the created path is a real directory.
func (c configuration) prepareExportRoot() error {
	if c.ExportRoot == "" {
		return nil
	}
	if err := os.MkdirAll(c.ExportRoot, 0o750); err != nil {
		return fmt.Errorf("create export_root: %w", err)
	}
	return c.validateExportRootKind()
}

// resourceSettings resolves the credential, preflight and restore settings.
// The KEK must sit outside the metadata, work and export directories, which is
// checked here so a misplacement is a startup error rather than a per-request
// failure.
func (c configuration) resourceSettings() (resources.Settings, error) {
	// The export root is created here, in the serve path only: migrate and user
	// administration never touch it, and a configured deployment must have a
	// real directory for the restore engine to validate and publish into.
	if err := c.prepareExportRoot(); err != nil {
		return resources.Settings{}, err
	}
	settings := resources.Settings{
		KeyFile: c.KEKFile, MetadataDir: c.MetadataDir, WorkRoot: c.WorkRoot,
		XtrabackupPath: c.XtrabackupPath, MinWorkBytes: c.MinWorkBytes, MinBackupBytes: c.MinBackupBytes,
		ExportRoot: c.ExportRoot, XbstreamPath: c.XbstreamPath, MysqldPath: c.MysqldPath,
		RestoreExpansionFactor: c.RestoreExpansionFactor, RestoreUseMemory: c.RestoreUseMemory,
	}
	if c.KEKFile == "" {
		return settings, nil
	}
	forbidden := []string{c.MetadataDir}
	if c.WorkRoot != "" {
		forbidden = append(forbidden, c.WorkRoot)
	}
	if c.ExportRoot != "" {
		forbidden = append(forbidden, c.ExportRoot)
	}
	sealer, err := credential.Open(c.KEKFile, forbidden...)
	if err != nil {
		return resources.Settings{}, err
	}
	settings.Sealer = sealer
	return settings, nil
}

// validateTransport rejects plain non-loopback listeners before any certificate
// read. tls_terminated_by_proxy is explicit and still requires trusted_proxies;
// the proxy list alone does not prove TLS. The mode is not end-to-end TLS:
// clients must be prevented from opening the backend port directly.
func (c configuration) validateTransport() error {
	if c.TLSTerminatedByProxy && len(c.TrustedProxies) == 0 {
		return errors.New("tls_terminated_by_proxy requires trusted_proxies and does not prove end-to-end TLS")
	}
	return server.RejectUnprotectedListen(c.Listen, c.TLS.CertFile != "", c.TLSTerminatedByProxy)
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
