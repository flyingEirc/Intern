import type { z } from "zod";
import type { RunLogEvent } from "@/lib/api/generated/types.gen";
import { zRunLogEvent } from "@/lib/api/generated/zod.gen";

/**
 * What one streamed frame means for the run detail page.
 *
 * `log` carries a contract log row. `end` is the stream's single final frame:
 * its data is the terminal status as plain text, so it fails the log schema on
 * purpose. `revoked` has no data; the session stopped being valid and the page
 * must hand over to the same expiry path a 401 uses. `ignore` covers heartbeat
 * comments (no event, no data) and anything else.
 */
export type RunStreamFrame =
  | { type: "log"; event: RunLogEvent }
  | { type: "end" }
  | { type: "revoked" }
  | { type: "ignore" };

export function interpretRunFrame(
  eventName: string | undefined,
  data: unknown,
): RunStreamFrame {
  if (eventName === "revoked") {
    return { type: "revoked" };
  }
  if (eventName === "end") {
    return { type: "end" };
  }
  const parsed = zRunLogEvent.safeParse(data);
  if (parsed.success) {
    return { type: "log", event: toRunLogEvent(parsed.data) };
  }
  return { type: "ignore" };
}

/**
 * The contract's sequence numbers are int64. Zod therefore validates them as
 * bigint, while the generated TypeScript type and the `after_seq` query
 * parameter are `number`. A run log is capped at 1000 rows, so the value is
 * always far inside the safe integer range; refuse anything that is not.
 */
export function sequenceNumber(value: bigint | number): number {
  const number = typeof value === "bigint" ? Number(value) : value;
  if (!Number.isSafeInteger(number) || number < 1) {
    throw new Error(`run log sequence is outside the safe integer range`);
  }
  return number;
}

function toRunLogEvent(data: z.infer<typeof zRunLogEvent>): RunLogEvent {
  return { ...data, seq: sequenceNumber(data.seq) };
}
