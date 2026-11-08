import { z } from "zod";
import type {
  Repository,
  RepositoryKind,
  RepositoryWriteWritable,
  TlsMode,
} from "@/lib/api/generated/types.gen";
import { hasControlCharacters } from "./validation";

export type RepositoryFormValues = {
  name: string;
  kind: RepositoryKind;
  tls_mode: TlsMode | "";
  root: string;
  host: string;
  port: string;
  base_path: string;
  username: string;
  password: string;
  ca_pem: string;
};

export const repositoryKindLabel: Record<RepositoryKind, string> = {
  local: "本地目录",
  ftp: "FTP / FTPS",
};

export const tlsModeLabel: Record<TlsMode, string> = {
  none: "无（本地目录）",
  plain: "明文 FTP",
  explicit: "显式 FTPS",
  implicit: "隐式 FTPS",
};

export const tlsModeChoiceLabel: Record<
  Exclude<TlsMode, "none">,
  { title: string; description: string }
> = {
  plain: {
    title: "明文 FTP",
    description: "凭据与数据都明文传输，仅限受信网络。",
  },
  explicit: {
    title: "显式 FTPS",
    description: "先连接 FTP，再用 AUTH TLS 升级到 TLS 1.2 及以上。",
  },
  implicit: {
    title: "隐式 FTPS",
    description: "直接连接只提供 TLS 的端口。",
  },
};

const absoluteFTPPath = /^(\/[^/]+)*$/;
const caPemLimit = 262144;

export function newRepositoryFormValues(): RepositoryFormValues {
  return {
    name: "",
    kind: "local",
    tls_mode: "none",
    root: "",
    host: "",
    port: "21",
    base_path: "",
    username: "",
    password: "",
    ca_pem: "",
  };
}

export function repositoryToFormValues(
  repository: Repository,
): RepositoryFormValues {
  return {
    name: repository.name,
    kind: repository.kind,
    tls_mode: repository.tls_mode,
    root: repository.local?.root ?? "",
    host: repository.ftp?.host ?? "",
    port: repository.ftp ? String(repository.ftp.port) : "21",
    base_path: repository.ftp?.base_path ?? "",
    username: repository.ftp?.username ?? "",
    password: "",
    ca_pem: repository.ftp?.ca_pem ?? "",
  };
}

/**
 * Form rules mirror the contract and the backend's validation messages. The
 * generated `zRepositoryWriteWritable` is parsed again before every request,
 * so a body the contract rejects never leaves the browser.
 */
export function repositoryFormSchema(creating: boolean) {
  return z
    .object({
      name: z
        .string()
        .trim()
        .min(1, "请输入仓库名称")
        .max(64, "仓库名称不能超过 64 个字符"),
      kind: z.enum(["local", "ftp"]),
      tls_mode: z.enum(["", "none", "plain", "explicit", "implicit"]),
      root: z.string(),
      host: z.string(),
      port: z.string(),
      base_path: z.string(),
      username: z.string(),
      password: z.string(),
      ca_pem: z.string(),
    })
    .superRefine((value, ctx) => {
      if (hasControlCharacters(value.name)) {
        ctx.addIssue({
          code: "custom",
          path: ["name"],
          message: "仓库名称不能包含控制字符",
        });
      }
      if (value.kind === "local") {
        if (value.tls_mode !== "none") {
          ctx.addIssue({
            code: "custom",
            path: ["tls_mode"],
            message: "本地目录的传输方式固定为 none",
          });
        }
        if (
          !value.root.trim().startsWith("/") ||
          hasControlCharacters(value.root)
        ) {
          ctx.addIssue({
            code: "custom",
            path: ["root"],
            message: "请输入绝对路径，例如 /backup/repository",
          });
        }
        return;
      }

      if (
        value.tls_mode !== "plain" &&
        value.tls_mode !== "explicit" &&
        value.tls_mode !== "implicit"
      ) {
        ctx.addIssue({
          code: "custom",
          path: ["tls_mode"],
          message: "请选择 FTP 传输方式：明文、显式 TLS 或隐式 TLS",
        });
      }
      if (value.host.trim() === "" || hasControlCharacters(value.host)) {
        ctx.addIssue({
          code: "custom",
          path: ["host"],
          message: "请输入 FTP 主机名或 IP 地址",
        });
      }
      const port = Number(value.port.trim());
      if (
        !/^\d+$/.test(value.port.trim()) ||
        !Number.isInteger(port) ||
        port < 1 ||
        port > 65535
      ) {
        ctx.addIssue({
          code: "custom",
          path: ["port"],
          message: "端口必须是 1–65535 的整数",
        });
      }
      if (!absoluteFTPPath.test(value.base_path.trim())) {
        ctx.addIssue({
          code: "custom",
          path: ["base_path"],
          message:
            "请输入绝对路径（以 / 开头，不能以 / 结尾或包含 //），例如 /backups",
        });
      }
      if (
        value.username.trim() === "" ||
        hasControlCharacters(value.username)
      ) {
        ctx.addIssue({
          code: "custom",
          path: ["username"],
          message: "请输入 FTP 用户名",
        });
      }
      if (hasControlCharacters(value.password)) {
        ctx.addIssue({
          code: "custom",
          path: ["password"],
          message: "密码不能包含控制字符",
        });
      }
      if (creating && value.password === "") {
        ctx.addIssue({
          code: "custom",
          path: ["password"],
          message: "新建 FTP 仓库时必须设置密码",
        });
      }
      const caPem = value.ca_pem.trim();
      if (caPem.length > caPemLimit) {
        ctx.addIssue({
          code: "custom",
          path: ["ca_pem"],
          message: "CA bundle 不能超过 262144 字节",
        });
      }
      if (caPem !== "" && value.tls_mode === "plain") {
        ctx.addIssue({
          code: "custom",
          path: ["ca_pem"],
          message: "明文 FTP 不能配置 CA bundle；请选择显式或隐式 FTPS",
        });
      }
    });
}

/**
 * Builds the wire body. `tls_mode` is always present: local is `none` and an
 * FTP repository carries the user's explicit choice, never a guessed default.
 * A blank password is omitted so an update keeps the stored secret.
 */
export function buildRepositoryWrite(
  value: RepositoryFormValues,
): RepositoryWriteWritable {
  const name = value.name.trim();
  if (value.kind === "local") {
    return {
      name,
      kind: "local",
      tls_mode: "none",
      root: value.root.trim(),
    };
  }
  const caPem = value.ca_pem.trim();
  return {
    name,
    kind: "ftp",
    tls_mode: value.tls_mode as Exclude<TlsMode, "none">,
    ftp: {
      host: value.host.trim(),
      port: Number(value.port.trim()),
      base_path: value.base_path.trim(),
      username: value.username.trim(),
      ...(caPem === "" ? {} : { ca_pem: caPem }),
    },
    ...(value.password === "" ? {} : { password: value.password }),
  };
}

export function repositoryEndpoint(repository: Repository): string {
  if (repository.kind === "local") {
    return repository.local?.root ?? "";
  }
  if (!repository.ftp) {
    return "";
  }
  const base = repository.ftp.base_path === "" ? "/" : repository.ftp.base_path;
  return `${repository.ftp.host}:${repository.ftp.port}${base}`;
}
