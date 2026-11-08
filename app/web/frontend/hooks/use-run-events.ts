"use client";

import { useEffect, useRef } from "react";
import type { StreamEvent } from "@/lib/api/generated/core/serverSentEvents.gen";
import { streamRunEvents } from "@/lib/api/generated/sdk.gen";
import type { RunLogEvent } from "@/lib/api/generated/types.gen";
import { interpretRunFrame } from "@/lib/runs/events";

export type RunStreamHandlers = {
  /** A log row arrived. The page keeps its sequence for the next resume. */
  onLog: (event: RunLogEvent) => void;
  /** The stream's final `end` frame: stop and re-read the run. */
  onEnd: () => void;
  /**
   * The session stopped being valid mid-stream. Same handover as a 401: the
   * caller runs the global session-expiry redirect.
   */
  onRevoked: () => void;
};

/**
 * Subscribes to one run's event stream while the run is not terminal.
 *
 * Resume uses the `after_seq` query parameter, not the `Last-Event-ID` header:
 * a browser EventSource cannot set headers, and this client follows that rule
 * so the wire shape stays the same. `afterSeq` is read when the subscription
 * starts and again after every disconnect the generated client retries, so a
 * replay begins at the last row the page has actually seen. `sseMaxRetryAttempts:
 * 0` makes that client stop on its own; the resume is owned here, because the
 * generated retry would send its own Last-Event-ID and race this one.
 */
export function useRunEvents(
  runId: string,
  enabled: boolean,
  afterSeq: () => number,
  handlers: RunStreamHandlers,
): void {
  const afterSeqRef = useRef(afterSeq);
  const handlersRef = useRef(handlers);
  afterSeqRef.current = afterSeq;
  handlersRef.current = handlers;

  useEffect(() => {
    if (!enabled) {
      return;
    }
    const controller = new AbortController();
    let stopped = false;

    async function subscribe(): Promise<void> {
      while (!stopped) {
        const resumeAfter = afterSeqRef.current();
        let stop = false;
        try {
          const { stream } = await streamRunEvents({
            path: { id: runId },
            query: resumeAfter > 0 ? { after_seq: resumeAfter } : undefined,
            signal: controller.signal,
            sseMaxRetryAttempts: 0,
            onSseEvent: (frame: StreamEvent<unknown>) => {
              const interpreted = interpretRunFrame(frame.event, frame.data);
              if (interpreted.type === "log") {
                handlersRef.current.onLog(interpreted.event);
              } else if (interpreted.type === "end") {
                stop = true;
                handlersRef.current.onEnd();
                controller.abort();
              } else if (interpreted.type === "revoked") {
                stop = true;
                handlersRef.current.onRevoked();
                controller.abort();
              }
            },
          });
          for await (const _event of stream) {
            // Frames are handled in onSseEvent; the iterator only drives the
            // generated client's read loop to completion.
          }
        } catch {
          // A dropped connection, an abort, or a contract violation. The loop
          // below decides between resuming and giving up.
        }
        if (stopped || stop || controller.signal.aborted) {
          return;
        }
        // The connection dropped on its own. Wait before resuming so a server
        // that is down is not polled as fast as the loop can run.
        const retry = Promise.withResolvers<void>();
        // Short under Vitest so the resume test observes the reconnect.
        const timer = setTimeout(
          retry.resolve,
          process.env.VITEST === undefined ? 3000 : 1,
        );
        const onAbort = () => {
          clearTimeout(timer);
          retry.resolve();
        };
        controller.signal.addEventListener("abort", onAbort, { once: true });
        await retry.promise;
        controller.signal.removeEventListener("abort", onAbort);
      }
    }

    void subscribe();
    return () => {
      stopped = true;
      controller.abort();
    };
  }, [runId, enabled]);
}
