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
	flags.String("listen", ":8080", "HTTP listen address (host:port)")
	flags.String("metadata-dir", "/var/lib/intern/metadata", "Private absolute metadata directory (chmod 700)")
	flags.String("log-level", "info", "JSON log level: debug, info, warn, error")
	flags.String("tls-cert-file", "", "TLS certificate PEM file (requires --tls-key-file)")
	flags.String("tls-key-file", "", "TLS private key PEM file (requires --tls-cert-file)")
	flags.Duration("shutdown-timeout", 30*time.Second, "Graceful shutdown deadline")
	flags.StringSlice("trusted-proxies", nil, "Trusted reverse proxy IPs or CIDRs (default: none)")
	flags.Duration("session-idle-timeout", 30*time.Minute, "Maximum session inactivity (must not exceed lifetime)")
	flags.Duration("session-lifetime", 12*time.Hour, "Absolute session lifetime")
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
	"session-idle-timeout": "session.idle_timeout", "session-lifetime": "session.lifetime",
}
