/**
 * A protection policy needs a local repository to store its first copy; an
 * offsite-only repository cannot hold one. Onboarding therefore counts only
 * local repositories as "storage ready".
 */
export function hasLocalRepository(
  repositories: ReadonlyArray<{ kind: string }>,
): boolean {
  return repositories.some((repository) => repository.kind === "local");
}
