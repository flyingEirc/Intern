package cmd

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/internal/artifact"
)

func cleanConfigEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"INTERN_CONFIG", "INTERN_LISTEN", "INTERN_METADATA_DIR", "INTERN_LOG_LEVEL", "INTERN_TLS_CERT_FILE", "INTERN_TLS_KEY_FILE", "INTERN_SHUTDOWN_TIMEOUT", "INTERN_TRUSTED_PROXIES", "INTERN_TLS_TERMINATED_BY_PROXY", "INTERN_SESSION_IDLE_TIMEOUT", "INTERN_SESSION_LIFETIME",
		"INTERN_KEK_FILE", "INTERN_WORK_ROOT", "INTERN_XTRABACKUP_PATH", "INTERN_MIN_WORK_BYTES", "INTERN_MIN_BACKUP_BYTES",
	}
	for _, key := range keys {
		value, existed := os.LookupEnv(key)
		t.Cleanup(func() {
			if existed {
				os.Setenv(key, value)
			} else {
				os.Unsetenv(key)
			}
		})
		os.Unsetenv(key)
	}
}

func loadConfig(t *testing.T, args ...string) (configuration, error) {
	t.Helper()
	loader := newConfigLoader(&cobra.Command{})
	if err := loader.flags.Parse(args); err != nil {
		t.Fatal(err)
	}
	return loader.load()
}

func TestConfigDefaults(t *testing.T) {
	cleanConfigEnv(t)
	c, err := loadConfig(t)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8080" || c.MetadataDir != "/var/lib/intern/metadata" || c.Log.Level != "info" || c.ShutdownTimeout != 30*time.Second || len(c.TrustedProxies) != 0 || c.TLSTerminatedByProxy {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestConfigPrecedence(t *testing.T) {
	cleanConfigEnv(t)
	path := filepath.Join(t.TempDir(), "web.yaml")
	if err := os.WriteFile(path, []byte("listen: 127.0.0.1:7001\nmetadata_dir: /yaml/metadata\nlog:\n  level: warn\nshutdown_timeout: 5s\ntrusted_proxies: [127.0.0.0/8]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INTERN_CONFIG", path)
	c, err := loadConfig(t)
	assertConfig(t, c, err, "127.0.0.1:7001", "/yaml/metadata", "warn", 5*time.Second, []string{"127.0.0.0/8"})
	t.Setenv("INTERN_LISTEN", "127.0.0.1:7002")
	t.Setenv("INTERN_METADATA_DIR", "/env/metadata")
	t.Setenv("INTERN_LOG_LEVEL", "debug")
	t.Setenv("INTERN_SHUTDOWN_TIMEOUT", "6s")
	t.Setenv("INTERN_TRUSTED_PROXIES", "192.0.2.1,198.51.100.0/24")
	c, err = loadConfig(t)
	assertConfig(t, c, err, "127.0.0.1:7002", "/env/metadata", "debug", 6*time.Second, []string{"192.0.2.1", "198.51.100.0/24"})
	t.Setenv("INTERN_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	c, err = loadConfig(t, "--config", path, "--listen", "127.0.0.1:7003", "--metadata-dir", "/flags/metadata", "--log-level", "error", "--shutdown-timeout", "7s", "--trusted-proxies", "203.0.113.1")
	assertConfig(t, c, err, "127.0.0.1:7003", "/flags/metadata", "error", 7*time.Second, []string{"203.0.113.1"})
}

func assertConfig(t *testing.T, c configuration, err error, listen, dir, level string, timeout time.Duration, proxies []string) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	want := configuration{Listen: listen, MetadataDir: dir, ShutdownTimeout: timeout, TrustedProxies: proxies}
	want.Log.Level = level
	want.Session = auth.DefaultSessionConfig()
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("configuration = %+v; want %+v", c, want)
	}
}

// TestOptionalResourceSettings pins the Stage C configuration contract: every new
// credential and preflight setting is optional and absent by default.
func TestOptionalResourceSettings(t *testing.T) {
	cleanConfigEnv(t)
	defaults, err := loadConfig(t)
	if err != nil {
		t.Fatal(err)
	}
	assertNoResourceSettings(t, defaults)
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "kek.json")
	loaded, err := loadConfig(t, "--kek-file", keyFile, "--work-root", filepath.Join(dir, "work"),
		"--xtrabackup-path", "/usr/bin/xtrabackup", "--min-work-bytes", "1024", "--min-backup-bytes", "2048")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.KEKFile != keyFile || loaded.MinWorkBytes != 1024 || loaded.MinBackupBytes != 2048 ||
		loaded.WorkRoot != filepath.Join(dir, "work") || loaded.XtrabackupPath != "/usr/bin/xtrabackup" {
		t.Fatalf("resources = %+v", loaded)
	}
}

func assertNoResourceSettings(t *testing.T, c configuration) {
	t.Helper()
	if c.KEKFile != "" || c.WorkRoot != "" || c.XtrabackupPath != "" || c.MinWorkBytes != 0 || c.MinBackupBytes != 0 {
		t.Fatalf("defaults = %+v", c)
	}
}

// TestOptionalResourceSettingsValidation requires each supplied value to be
// absolute, and the two capacity thresholds to be supplied together.
func TestOptionalResourceSettingsValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		args    []string
		message string
	}{
		"relative kek":        {[]string{"--kek-file", "relative/kek.json"}, "kek_file must be an absolute path"},
		"relative work":       {[]string{"--work-root", "work"}, "work_root must be an absolute path"},
		"relative xtrabackup": {[]string{"--xtrabackup-path", "xtrabackup"}, "xtrabackup_path must be an absolute path"},
		"work without backup": {[]string{"--min-work-bytes", "1024"}, "min_backup_bytes must be positive"},
		"backup without work": {[]string{"--min-backup-bytes", "1024"}, "min_work_bytes must be positive"},
	} {
		t.Run(name, func(t *testing.T) {
			cleanConfigEnv(t)
			_, err := loadConfig(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error = %v, want %q", err, tc.message)
			}
		})
	}
}

// TestKEKInsideMetadataIsRefusedAtStartup covers the location rule that only
// matters when the key exists and the server is starting.
func TestKEKInsideMetadataIsRefusedAtStartup(t *testing.T) {
	cleanConfigEnv(t)
	metadata := privateConfigDir(t)
	inside := filepath.Join(metadata, "kek.json")
	if err := artifact.GenerateKey(inside); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(t, "--metadata-dir", metadata, "--kek-file", inside)
	if err != nil {
		t.Fatalf("configuration load refused an optional key: %v", err)
	}
	if _, err = c.resourceSettings(); err == nil {
		t.Fatal("a KEK inside metadata_dir was accepted")
	}
}

// TestKEKOutsideMetadataEnablesCredentials covers the accepted location: a key
// outside the metadata directory enables sealing but not the physical preflight.
func TestKEKOutsideMetadataEnablesCredentials(t *testing.T) {
	cleanConfigEnv(t)
	metadata := privateConfigDir(t)
	outside := filepath.Join(t.TempDir(), "kek.json")
	if err := artifact.GenerateKey(outside); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(t, "--metadata-dir", metadata, "--kek-file", outside)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := c.resourceSettings()
	if err != nil {
		t.Fatalf("an outside KEK was refused: %v", err)
	}
	if !settings.Credentials() || settings.Preflight() {
		t.Fatalf("settings = %+v", settings)
	}
}

// TestEmptyWorkRootDoesNotResolveToCwdForKEK proves an omitted work_root is not
// passed to credential.Open, so a KEK sitting in the process cwd is accepted.
func TestEmptyWorkRootDoesNotResolveToCwdForKEK(t *testing.T) {
	cleanConfigEnv(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	keyFile := filepath.Join(cwd, "kek.json")
	if err := artifact.GenerateKey(keyFile); err != nil {
		t.Fatal(err)
	}
	metadata := privateConfigDir(t)
	c, err := loadConfig(t, "--metadata-dir", metadata, "--kek-file", keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkRoot != "" {
		t.Fatalf("work_root = %q", c.WorkRoot)
	}
	settings, err := c.resourceSettings()
	if err != nil || !settings.Credentials() {
		t.Fatalf("empty work_root treated as cwd: %+v %v", settings, err)
	}
}

// TestAbsentKEKDisablesOnlyDependentOperations proves an absent key is not a
// startup failure.
func TestAbsentKEKDisablesOnlyDependentOperations(t *testing.T) {
	cleanConfigEnv(t)
	metadata := privateConfigDir(t)
	c, err := loadConfig(t, "--metadata-dir", metadata)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := c.resourceSettings()
	if err != nil || settings.Credentials() || settings.Preflight() {
		t.Fatalf("unconfigured settings = %+v %v", settings, err)
	}
}

func privateConfigDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "metadata")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestConfigErrorsHaveNoSideEffects(t *testing.T) {
	cases := []struct {
		name, message string
		args          []string
		setup         func(*testing.T) []string
	}{
		{"listen", "listen", []string{"--listen", "invalid"}, nil},
		{"port", "listen", []string{"--listen", "127.0.0.1:no"}, nil},
		{"port range", "listen", []string{"--listen", "127.0.0.1:65536"}, nil},
		{"invalid host", "listen", []string{"--listen", "bad host:8080"}, nil},
		{"invalid IPv6", "listen", []string{"--listen", "[:::]:8080"}, nil},
		{"empty metadata", "metadata_dir", []string{"--metadata-dir", ""}, nil},
		{"relative metadata", "metadata_dir", []string{"--metadata-dir", "relative"}, nil},
		{"log level", "log.level", []string{"--log-level", "verbose"}, nil},
		{"timeout", "shutdown_timeout", []string{"--shutdown-timeout", "0s"}, nil},
		{"session idle", "session.idle_timeout", []string{"--session-idle-timeout", "0s"}, nil},
		{"session lifetime", "session.lifetime", []string{"--session-lifetime", "-1s"}, nil},
		{"session ordering", "idle_timeout <= lifetime", []string{"--session-idle-timeout", "13h"}, nil},
		{"cert without key", "supplied together", []string{"--tls-cert-file", "/missing/cert"}, nil},
		{"key without cert", "supplied together", []string{"--tls-key-file", "/missing/key"}, nil},
		{"unreadable TLS", "cannot load TLS", []string{"--tls-cert-file", "/missing/cert", "--tls-key-file", "/missing/key"}, nil},
		{"configuration", "cannot open YAML", []string{"--config", "/missing/config.yaml"}, nil},
		{"proxy", "trusted_proxies", []string{"--trusted-proxies", "not-an-ip"}, nil},
		{"plain non-loopback", "non-loopback", []string{"--listen", "0.0.0.0:8080"}, nil},
		{"proxies are not TLS", "non-loopback", []string{"--listen", "192.0.2.10:8080", "--trusted-proxies", "192.0.2.1"}, nil},
		{"proxy mode without proxies", "does not prove end-to-end TLS", []string{"--tls-terminated-by-proxy"}, nil},
		{"zone proxy", "trusted_proxies", []string{"--trusted-proxies", "fe80::1%lo0"}, nil},
		{"zone proxy CIDR", "trusted_proxies", []string{"--trusted-proxies", "fe80::1%lo0/64"}, nil},
		{"config directory", "regular file", nil, configDirectoryFlag},
		{"config directory env", "regular file", nil, configDirectoryEnv},
		{"config fifo", "regular file", nil, configFIFO},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cleanConfigEnv(t)
			dir := filepath.Join(t.TempDir(), "metadata")
			addr := freeListenAddr(t)
			extra := tc.args
			if tc.setup != nil {
				extra = append(tc.setup(t), extra...)
			}
			args := append([]string{"serve", "--metadata-dir", dir, "--listen", addr}, extra...)
			command := New(io.Discard, io.Discard)
			command.SetArgs(args)
			err := executeConfigError(t, command, addr)
			if err == nil || !strings.Contains(err.Error(), tc.message) || strings.Contains(err.Error(), "\n") {
				t.Fatalf("error = %q, want one line containing %q", err, tc.message)
			}
			if _, statErr := os.Lstat(dir); !os.IsNotExist(statErr) {
				t.Fatalf("configuration created metadata directory: %v", statErr)
			}
			if conn, dialErr := net.DialTimeout("tcp", addr, 50*time.Millisecond); dialErr == nil {
				conn.Close()
				t.Fatal("configuration error left a listener")
			}
		})
	}
}

func configDirectoryFlag(t *testing.T) []string {
	t.Helper()
	return []string{"--config", t.TempDir()}
}

func configDirectoryEnv(t *testing.T) []string {
	t.Helper()
	t.Setenv("INTERN_CONFIG", t.TempDir())
	return nil
}

func freeListenAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// executeConfigError bounds a configuration failure that incorrectly starts serving.
// Both waits are limited, so a blocking open fails the test instead of hanging it.
func executeConfigError(t *testing.T, command *cobra.Command, addr string) error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- command.ExecuteContext(ctx) }()
	const deadline = time.Second
	select {
	case err := <-errCh:
		return err
	case <-time.After(deadline):
		conn, dialErr := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		listening := dialErr == nil
		if listening {
			conn.Close()
		}
		cancel()
		select {
		case err := <-errCh:
			if listening {
				return errors.New("configuration error started a listener")
			}
			return err
		case <-time.After(deadline):
			if listening {
				return errors.New("configuration error started a listener")
			}
			return errors.New("configuration error blocked before returning")
		}
	}
}

func TestTrustedProxyAcceptance(t *testing.T) {
	for _, proxy := range []string{"192.0.2.1", "192.0.2.0/24", "192.0.2.1/24", "2001:db8::1", "2001:db8::/32", "::1", "::ffff:192.0.2.1"} {
		if err := validateProxies([]string{proxy}); err != nil {
			t.Fatalf("proxy %q: %v", proxy, err)
		}
	}
	for _, proxy := range []string{"fe80::1%lo0", "fe80::1%lo0/64", "not-an-ip", "localhost", "192.0.2.0/33"} {
		err := validateProxies([]string{proxy})
		if err == nil || !strings.Contains(err.Error(), "trusted_proxies") {
			t.Fatalf("proxy %q error = %v", proxy, err)
		}
	}
}

func TestUnknownConfigurationRejected(t *testing.T) {
	cleanConfigEnv(t)
	path := filepath.Join(t.TempDir(), "web.yaml")
	if err := os.WriteFile(path, []byte("unknown: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(t, "--config", path); err == nil || !strings.Contains(err.Error(), "configuration key") {
		t.Fatalf("unknown configuration accepted: %v", err)
	}
	t.Setenv("INTERN_METADATA_DIR", "")
	if _, err := loadConfig(t); err == nil || !strings.Contains(err.Error(), "metadata_dir") {
		t.Fatalf("empty environment ignored: %v", err)
	}
}

func TestListenAddressSyntax(t *testing.T) {
	for _, tc := range []struct {
		address string
		valid   bool
	}{
		{":8080", true},
		{"127.0.0.1:0", true},
		{"[::1]:8080", true},
		{"[fe80::1%lo0]:8080", true},
		{"localhost:8080", true},
		{"intern-1.example.:8080", true},
		{"bad host:8080", false},
		{"[:::]:8080", false},
		{"-bad.example:8080", false},
		{"bad-.example:8080", false},
		{"bad..example:8080", false},
		{"localhost:", false},
		{strings.Repeat("a", 64) + ".example:8080", false},
	} {
		t.Run(tc.address, func(t *testing.T) {
			if err := validateListen(tc.address); (err == nil) != tc.valid {
				t.Fatalf("listen %q: valid=%v, error=%v", tc.address, tc.valid, err)
			}
		})
	}
}

func TestPlainListenPolicy(t *testing.T) {
	cleanConfigEnv(t)
	c, err := loadConfig(t, "--listen", "localhost:8080")
	if err != nil || c.Listen != "localhost:8080" {
		t.Fatalf("localhost listen: %v %+v", err, c)
	}
	if _, err = loadConfig(t, "--listen", "[::1]:9090"); err != nil {
		t.Fatal(err)
	}
	resolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		t.Errorf("listen validation resolved DNS")
		return nil, errors.New("resolved")
	}}
	t.Cleanup(func() { net.DefaultResolver = resolver })
	start := time.Now()
	_, err = loadConfig(t, "--listen", "unresolvable.invalid:8080")
	if err == nil || !strings.Contains(err.Error(), "non-loopback") {
		t.Fatalf("hostname listen: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("hostname check took %s", time.Since(start))
	}
	c, err = loadConfig(t, "--listen", "192.0.2.10:8443", "--tls-terminated-by-proxy", "--trusted-proxies", "192.0.2.1")
	assertProxyTerminatedListen(t, c, err)
}

func assertProxyTerminatedListen(t *testing.T, c configuration, err error) {
	t.Helper()
	if err != nil || !c.TLSTerminatedByProxy || len(c.TrustedProxies) != 1 || c.TrustedProxies[0] != "192.0.2.1" {
		t.Fatalf("proxy mode: %v %+v", err, c)
	}
}
