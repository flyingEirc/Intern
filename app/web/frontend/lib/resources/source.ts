import { z } from "zod";
import type {
  Source,
  SourceWriteWritable,
} from "@/lib/api/generated/types.gen";
import { hasControlCharacters } from "./validation";

export type SourceFormValues = {
  name: string;
  socket: string;
  datadir: string;
  username: string;
  password: string;
};

export function newSourceFormValues(): SourceFormValues {
  return { name: "", socket: "", datadir: "", username: "", password: "" };
}

export function sourceToFormValues(source: Source): SourceFormValues {
  return {
    name: source.name,
    socket: source.socket,
    datadir: source.datadir,
    username: source.username,
    password: "",
  };
}

/** Form rules mirror the contract; the generated schema is parsed again on submit. */
export function sourceFormSchema(creating: boolean) {
  return z
    .object({
      name: z
        .string()
        .trim()
        .min(1, "请输入数据源名称")
        .max(64, "数据源名称不能超过 64 个字符"),
      socket: z.string(),
      datadir: z.string(),
      username: z.string(),
      password: z.string(),
    })
    .superRefine((value, ctx) => {
      if (hasControlCharacters(value.name)) {
        ctx.addIssue({
          code: "custom",
          path: ["name"],
          message: "数据源名称不能包含控制字符",
        });
      }
      if (
        !value.socket.trim().startsWith("/") ||
        hasControlCharacters(value.socket)
      ) {
        ctx.addIssue({
          code: "custom",
          path: ["socket"],
          message:
            "请输入 MySQL socket 的绝对路径，例如 /run/mysqld/mysqld.sock",
        });
      }
      if (
        !value.datadir.trim().startsWith("/") ||
        hasControlCharacters(value.datadir)
      ) {
        ctx.addIssue({
          code: "custom",
          path: ["datadir"],
          message: "请输入 MySQL datadir 的绝对路径，例如 /var/lib/mysql",
        });
      }
      if (
        value.username.trim() === "" ||
        hasControlCharacters(value.username)
      ) {
        ctx.addIssue({
          code: "custom",
          path: ["username"],
          message: "请输入 MySQL 用户名",
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
          message: "新建数据源时必须设置密码",
        });
      }
    });
}

/** A blank password is omitted so an update keeps the stored secret. */
export function buildSourceWrite(value: SourceFormValues): SourceWriteWritable {
  return {
    name: value.name.trim(),
    socket: value.socket.trim(),
    datadir: value.datadir.trim(),
    username: value.username.trim(),
    ...(value.password === "" ? {} : { password: value.password }),
  };
}
