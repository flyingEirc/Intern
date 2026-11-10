/**
 * Business wording for the preflight engine's machine codes. The UI and the
 * end-to-end suite share this one table, so a check renamed for operators is
 * checked under the same name the page renders.
 */

/** Business names for the preflight check codes the engine records. */
export const preflightCheckLabel: Record<string, string> = {
  identity: "数据库身份",
  compatibility: "版本兼容",
  volume: "数据卷",
  mount: "挂载",
  privileges: "权限",
  datafiles: "数据文件",
  tables: "表",
  keyring: "加密密钥",
  page_tracking: "页跟踪",
  capacity: "可用空间",
};

/** Name the UI and the e2e suite use for a check code the table lacks. */
export const preflightUnknownCheckLabel = "其他检查项";

/** Business wording for the advisory warnings the engine emits. */
export const preflightWarningText: Record<string, string> = {
  compatibility_candidate_unverified: "当前 MySQL/工具版本组合尚未完成正式验证",
  table_inventory_partial:
    "备份账号无法列出全部表，非 InnoDB 表可能需要更强锁并整表复制",
};

/** Business name for a check code; unknown codes keep a generic name. */
export function checkLabel(code: string): string {
  return preflightCheckLabel[code] ?? preflightUnknownCheckLabel;
}

/** Business wording for a warning; unknown codes keep the engine message. */
export function warningText(code: string, message: string): string {
  return preflightWarningText[code] ?? message;
}
