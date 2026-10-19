package cmd

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

type configLoader struct {
	v       *viper.Viper
	flags   *pflag.FlagSet
	stderr  io.Writer
	initErr error
}

func newConfigLoader(root *cobra.Command, stderr io.Writer) *configLoader {
	home, homeErr := os.UserHomeDir()
	configDir, configErr := os.UserConfigDir()
	v := viper.New()
	v.SetEnvPrefix("INTERN")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	v.AutomaticEnv()
	v.AllowEmptyEnv(true)
	flags := root.PersistentFlags()
	connectionFlags(flags)
	backupFlags(flags, home, configDir)
	scheduleFlags(flags)
	loader := &configLoader{v: v, flags: flags, stderr: stderr, initErr: errors.Join(homeErr, configErr)}
	for flag, key := range flagBindings {
		loader.initErr = errors.Join(loader.initErr, v.BindPFlag(key, flags.Lookup(flag)))
	}
	return loader
}

func connectionFlags(flags *pflag.FlagSet) {
	flags.String("config", "", "YAML 配置路径（或 INTERN_CONFIG）")
	flags.String("host", "127.0.0.1", "MySQL 主机")
	flags.Int("port", 3306, "MySQL 端口")
	flags.String("socket", "", "MySQL Unix socket（优先于 host/port）")
	flags.String("username", "", "MySQL username")
	flags.String("password", "", "MySQL password；仅 --password 时隐藏输入，直接传值可能泄漏到 shell 历史")
	flags.Lookup("password").NoOptDefVal = "__INTERN_PROMPT__"
	flags.String("password-env", "", "存放 MySQL 密码的环境变量名")
	flags.String("db", "", "源库名；restore 命令中为目标库名")
	flags.String("tls-mode", "", "VERIFY_IDENTITY（TCP 默认）或显式 DISABLED（socket 默认）")
	flags.String("tls-ca", "", "VERIFY_IDENTITY 的 CA PEM 路径")
}

func backupFlags(flags *pflag.FlagSet, home, configDir string) {
	flags.String("directory", filepath.Join(home, "intern-backups"), "备份目录，要求 chmod 700")
	flags.String("state-directory", filepath.Join(configDir, "intern", "state"), "独立元数据目录，要求 chmod 700；需随备份另行保护")
	flags.String("project", "default", "项目隔离标识")
	flags.Bool("compress", true, "启用 zstd；使用 --compress=false 关闭")
	flags.Bool("encrypt", false, "启用 AES-128-GCM；需要 --key-file")
	flags.String("key-file", "", "独立 KEK 文件（chmod 600），不可放入备份或元数据目录")
	flags.Int64("max-restore-bytes", 100<<30, "恢复展开大小上限（字节）；恢复前需保留足够暂存磁盘")
}

func scheduleFlags(flags *pflag.FlagSet) {
	flags.String("full-at", "02:00", "每天全量时间 HH:MM")
	flags.Duration("incremental-every", 15*time.Minute, "增量间隔，如 30s、15m、2h")
	flags.String("timezone", "UTC", "每日调度的 IANA 时区，如 Asia/Shanghai")
}

var flagBindings = map[string]string{
	"host": "connection.host", "port": "connection.port", "socket": "connection.socket", "username": "connection.username", "password": "connection.password", "password-env": "connection.password_env", "db": "connection.db", "tls-mode": "connection.tls_mode", "tls-ca": "connection.tls_ca",
	"directory": "backup.directory", "state-directory": "backup.state_directory", "project": "backup.project", "compress": "backup.compress", "encrypt": "backup.encrypt", "key-file": "backup.key_file", "max-restore-bytes": "backup.max_restore_bytes",
	"full-at": "schedule.full_at", "incremental-every": "schedule.incremental_every", "timezone": "schedule.timezone",
}
