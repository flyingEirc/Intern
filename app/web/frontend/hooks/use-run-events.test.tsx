import { renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { client } from "@/lib/api/client";
import { fetchMock } from "@/test/render";
import { useRunEvents } from "./use-run-events";

function streamResponse(frames: string[]): Response {
  const encoder = new TextEncoder();
  return new Response(
    new ReadableStream({
      start(controller) {
        for (const frame of frames) {
          controller.enqueue(encoder.encode(frame));
        }
        controller.close();
      },
    }),
    {
      status: 200,
      headers: { "Content-Type": "text/event-stream" },
    },
  );
}

const row = (seq: number) =>
  `event: message\nid: ${seq}\ndata: {"seq":${seq},"ts":"2026-11-06T08:00:0${seq}Z","level":"info","event":"started","message":"step ${seq}","data":null}\n\n`;

afterEach(() => {
  fetchMock.mockReset();
});

function mount(
  handlers: {
    onLog?: (seq: number) => void;
    onEnd?: () => void;
    onRevoked?: () => void;
  },
  afterSeq: () => number,
) {
  client.setConfig({ baseUrl: "http://intern.test", fetch: fetchMock });
  return renderHook(() =>
    useRunEvents("run-1", true, afterSeq, {
      onLog: (event) => handlers.onLog?.(event.seq),
      onEnd: () => handlers.onEnd?.(),
      onRevoked: () => handlers.onRevoked?.(),
    }),
  );
}

function eventRequests(): Array<{ url: URL; headers: Headers }> {
  return fetchMock.mock.calls.map(([input]) => {
    const request = input as Request;
    return { url: new URL(request.url), headers: request.headers };
  });
}

describe("useRunEvents", () => {
  it("resumes after the last seen sequence and stops on end", async () => {
    let seen = 0;
    fetchMock
      .mockResolvedValueOnce(streamResponse([row(1), row(2)]))
      .mockResolvedValueOnce(
        streamResponse([row(3), "event: end\ndata: completed\n\n"]),
      );
    const onLog = vi.fn((seq: number) => {
      seen = seq;
    });
    const onEnd = vi.fn();

    mount({ onLog, onEnd }, () => seen);

    await waitFor(() => expect(onEnd).toHaveBeenCalledTimes(1));
    expect(onLog.mock.calls.map(([seq]) => seq)).toEqual([1, 2, 3]);

    const requests = eventRequests();
    expect(requests).toHaveLength(2);
    expect(requests[0]?.url.searchParams.has("after_seq")).toBe(false);
    expect(requests[1]?.url.searchParams.get("after_seq")).toBe("2");
    for (const request of requests) {
      expect(request.url.pathname).toBe("/api/v1/runs/run-1/events");
      expect(request.headers.get("Last-Event-ID")).toBeNull();
    }
    // end closed the stream: no third connection.
    const first = Promise.withResolvers<void>();
    setTimeout(first.resolve, 20);
    await first.promise;
    expect(eventRequests()).toHaveLength(2);
  });

  it("hands a revoked event to the session-expiry path and stops", async () => {
    fetchMock.mockResolvedValueOnce(
      streamResponse([row(4), "event: revoked\n\n"]),
    );
    const onRevoked = vi.fn();

    mount({ onRevoked }, () => 0);

    await waitFor(() => expect(onRevoked).toHaveBeenCalledTimes(1));
    const second = Promise.withResolvers<void>();
    setTimeout(second.resolve, 20);
    await second.promise;
    expect(eventRequests()).toHaveLength(1);
  });
});
