package cmd

import (
	"errors"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

type configLoader struct {
	v       *viper.Viper
	flags   *pflag.FlagSet
	initErr error
}

func newConfigLoader(root *cobra.Command) *configLoader {
	v := viper.New()
	v.SetEnvPrefix("INTERN")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	v.AutomaticEnv()
	v.AllowEmptyEnv(true)
	flags := root.PersistentFlags()
	flags.String("config", "", "YAML configuration path (or INTERN_CONFIG)")
	flags.String("listen", "127.0.0.1:8080", "Loopback HTTP listen address (host:port). Non-loopback requires TLS or --tls-terminated-by-proxy")
	flags.String("metadata-dir", "/var/lib/intern/metadata", "Private absolute metadata directory (chmod 700)")
	flags.String("log-level", "info", "JSON log level: debug, info, warn, error")
	flags.String("tls-cert-file", "", "TLS certificate PEM file (requires --tls-key-file)")
	flags.String("tls-key-file", "", "TLS private key PEM file (requires --tls-cert-file)")
	flags.Duration("shutdown-timeout", 30*time.Second, "Graceful shutdown window only. After it, in-flight handlers may still drain for the detached-audit budget plus the SQLite busy timeout plus a small margin (currently 10.5s). This is not an absolute process-exit deadline. Canceling a request does not interrupt a SQLite busy wait or fsync.")
	flags.StringSlice("trusted-proxies", nil, "Trusted reverse proxy IPs or CIDRs (default: none)")
	flags.Bool("tls-terminated-by-proxy", false, "Allow plain HTTP when a reverse proxy terminates TLS. Requires --trusted-proxies. Firewall direct access; this is not end-to-end TLS")
	flags.Duration("session-idle-timeout", 30*time.Minute, "Maximum session inactivity (must not exceed lifetime)")
	flags.Duration("session-lifetime", 12*time.Hour, "Absolute session lifetime")
	flags.String("kek-file", "", "KEK keyset that seals source and FTP repository passwords (must be outside metadata_dir and work_root)")
	flags.String("work-root", "", "Private absolute scratch root for physical preflight and backups")
	flags.String("xtrabackup-path", "", "Absolute path to the XtraBackup executable the preflight probes")
	flags.Uint64("min-work-bytes", 0, "Minimum free bytes required on the work volume (positive when set)")
	flags.Uint64("min-backup-bytes", 0, "Minimum free bytes required on the local repository volume (positive when set)")
	loader := &configLoader{v: v, flags: flags}
	for flag, key := range flagBindings {
		loader.initErr = errors.Join(loader.initErr, v.BindPFlag(key, flags.Lookup(flag)))
	}
	return loader
}

var flagBindings = map[string]string{
	"listen": "listen", "metadata-dir": "metadata_dir", "log-level": "log.level",
	"tls-cert-file": "tls.cert_file", "tls-key-file": "tls.key_file",
	"shutdown-timeout": "shutdown_timeout", "trusted-proxies": "trusted_proxies",
	"tls-terminated-by-proxy": "tls_terminated_by_proxy",
	"session-idle-timeout":    "session.idle_timeout", "session-lifetime": "session.lifetime",
	"kek-file": "kek_file", "work-root": "work_root", "xtrabackup-path": "xtrabackup_path",
	"min-work-bytes": "min_work_bytes", "min-backup-bytes": "min_backup_bytes",
}
