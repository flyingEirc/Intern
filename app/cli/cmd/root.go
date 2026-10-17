package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/flyingEirc/IotProject/internal/backup"
	"github.com/flyingEirc/IotProject/internal/fsutil"
)

type application struct {
	config      *configLoader
	out, stderr io.Writer
}

func New(out, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{Use: "intern", Short: "MySQL 逻辑全量、binlog 增量备份与隔离恢复", SilenceUsage: true, SilenceErrors: true, Long: rootHelp, Example: rootExamples}
	root.SetOut(out)
	root.SetErr(stderr)
	app := &application{config: newConfigLoader(root, stderr), out: out, stderr: stderr}
	root.AddCommand(app.backupCommand(), app.restoreCommand(), app.keygenCommand(), app.scheduleCommand())
	return root
}
func (a *application) emit(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	n, err := a.out.Write(data)
	if n != len(data) {
		return errors.Join(err, io.ErrShortWrite)
	}
	return err
}
func (a *application) withEngine(command *cobra.Command, fn func(*backup.Engine, configuration) error) (retErr error) {
	c, err := a.config.load()
	if err != nil {
		return err
	}
	engine, err := backup.Open(command.Context(), c.options())
	if err != nil {
		return err
	}
	defer fsutil.Close(&retErr, engine)
	return fn(engine, c)
}
func Execute(ctx context.Context) error { return New(os.Stdout, os.Stderr).ExecuteContext(ctx) }

const rootHelp = `Intern: MySQL 逻辑全量与日志增量备份（不是 XtraBackup 物理增量）。
配置优先级: 显式 flags > INTERN_ 环境变量 > YAML > 默认值。
示例环境变量: INTERN_CONNECTION_USERNAME、INTERN_BACKUP_COMPRESS。
密码推荐 password_env 引用；含明文密码的 YAML 必须 chmod 600。
加密使用独立 KEK + Tink AES-128-GCM，压缩使用 zstd（先压缩后加密）。
所有备份/恢复临时文件均需受保护的磁盘；不保证完全无明文落盘。
需要安装同主次版本 mysqldump/mysqlbinlog/mysql；不要在全量期间执行 DDL。
增量执行 FLUSH BINARY LOGS，要求轮转与复制读取权限；不会自动清理备份。
只恢复到不同 server_uuid 的空目标库。恢复失败可能留下部分 SQL 副作用。`
const rootExamples = `  intern keygen --key-file /secure/intern.key
  intern backup full --config ./backup.yaml
  intern backup incremental --config ./backup.yaml
  intern schedule run --config ./backup.yaml --full-at 02:00 --incremental-every 15m --timezone Asia/Shanghai
  intern restore --config ./target.yaml --backup BACKUP_ID
  intern restore --config ./target.yaml --backup BACKUP_ID --execute`
