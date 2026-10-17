package cmd

import (
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/backup"
	"github.com/flyingEirc/Intern/internal/schedule"
)

func (a *application) backupCommand() *cobra.Command {
	command := &cobra.Command{Use: "backup", Short: "创建或列出备份"}
	command.AddCommand(a.createBackupCommand("full", "逻辑全量（InnoDB 一致性快照，包含 routines/events/triggers）"), a.createBackupCommand("incremental", "封存并归档上个备份之后的 binlog 增量"))
	command.AddCommand(&cobra.Command{Use: "list", Short: "列出已完成发布的备份", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return a.withEngine(cmd, func(e *backup.Engine, _ configuration) error {
			r, err := e.List(cmd.Context())
			if err != nil {
				return err
			}
			return a.emit(r)
		})
	}})
	return command
}
func (a *application) createBackupCommand(kind, description string) *cobra.Command {
	return &cobra.Command{Use: kind, Short: description, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return a.withEngine(cmd, func(e *backup.Engine, _ configuration) error {
			var r backup.Result
			var err error
			if kind == "full" {
				r, err = e.Full(cmd.Context(), "")
			} else {
				r, err = e.Incremental(cmd.Context(), "")
			}
			if err != nil {
				return err
			}
			return a.emit(r)
		})
	}}
}
func (a *application) restoreCommand() *cobra.Command {
	var backupID string
	var execute bool
	command := &cobra.Command{Use: "restore", Short: "验证完整链并生成隔离目标恢复计划；--execute 才写入", Args: cobra.NoArgs, Long: "从选定备份恢复：若选择增量，自动应用全量及全部父增量。连接参数/YAML 为目标连接，不需要源库在线。默认只校验并输出计划；--execute 才执行。恢复到已封存增量的事务边界，不接受任意时间猜测。改库名使用官方 rewrite-db；含视图/存储程序/触发器的全量须在隔离目标保持原库名，不能安全改写任意 SQL。", RunE: func(cmd *cobra.Command, _ []string) error {
		return a.withEngine(cmd, func(e *backup.Engine, c configuration) error {
			plan, err := e.Restore(cmd.Context(), backup.RestoreRequest{BackupID: backupID, Target: c.Connection, Execute: execute})
			if err != nil {
				return err
			}
			return a.emit(plan)
		})
	}}
	command.Flags().StringVar(&backupID, "backup", "", "backup list 中的已发布备份 ID")
	command.Flags().BoolVar(&execute, "execute", false, "验证通过后执行恢复（默认只生成计划）")
	if err := command.MarkFlagRequired("backup"); err != nil {
		panic(err)
	}
	return command
}
func (a *application) keygenCommand() *cobra.Command {
	return &cobra.Command{Use: "keygen", Short: "创建随机 AES-128 KEK；请独立备份密钥，丢失后无法恢复加密备份", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		c, err := a.config.load()
		if err != nil {
			return err
		}
		if err = artifact.ValidateKeyLocation(c.Backup.KeyFile, c.Backup.Directory, c.Backup.StateDirectory); err != nil {
			return err
		}
		if err = artifact.GenerateKey(c.Backup.KeyFile); err != nil {
			return err
		}
		id, err := artifact.KeyID(c.Backup.KeyFile)
		if err != nil {
			return err
		}
		return a.emit(map[string]string{"key_file": c.Backup.KeyFile, "key_id": id, "algorithm": "aes-128-gcm"})
	}}
}
func (a *application) scheduleCommand() *cobra.Command {
	command := &cobra.Command{Use: "schedule", Short: "配置每日全量与间隔增量的前台常驻调度"}
	command.AddCommand(&cobra.Command{Use: "run", Short: "前台运行；首次建立全量基线，退出即停止调度", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return a.withEngine(cmd, func(e *backup.Engine, c configuration) error {
			logger := slog.New(slog.NewJSONHandler(a.stderr, nil))
			return schedule.Run(cmd.Context(), e, c.options(), c.Schedule, logger, func(r backup.Result) error { return a.emit(r) })
		})
	}})
	return command
}
