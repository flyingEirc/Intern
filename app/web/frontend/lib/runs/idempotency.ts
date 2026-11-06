/** One manual trigger submission: the key travels with every retry of it. */
export type TriggerAttempt = {
  key: string;
  kind: "full" | "incremental";
};

/**
 * The key of the click currently in flight, or a fresh one. A repeated click
 * and an automatic retry both reuse it, so the server's idempotency contract
 * answers with the same run instead of creating a second one. The next click,
 * once nothing is in flight, gets its own key.
 */
export function triggerAttempt(
  current: TriggerAttempt | null,
  kind: "full" | "incremental",
  freshKey: () => string,
): TriggerAttempt {
  if (current !== null && current.kind === kind) {
    return current;
  }
  return { key: freshKey(), kind };
}
