import type { Permission } from "@/lib/api/generated/types.gen";

/** Permissions the management pages need, by resource. */
export const managePermission = {
  repositories: "repositories:manage",
  sources: "sources:manage",
  policies: "policies:manage",
} as const satisfies Record<string, Permission>;

export function hasPermission(
  permissions: readonly Permission[] | undefined,
  permission: Permission,
): boolean {
  return permissions?.includes(permission) ?? false;
}
