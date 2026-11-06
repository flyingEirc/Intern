import type { Role } from "./api/generated/types.gen";

export const roleLabel: Record<Role, string> = {
  admin: "管理员",
  operator: "操作员",
  viewer: "只读",
};
