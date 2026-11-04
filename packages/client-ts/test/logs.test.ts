import { test } from "node:test";
import assert from "node:assert/strict";
import { createHarnessKit, type Transport } from "../src/index.ts";

async function unresponsiveLogsPeer() {
  const sent: { id?: number; method: string; params?: { id?: number } }[] = [];
  let receive: (line: string) => void = () => {};
  let closed: () => void = () => {};
  const transport: Transport = {
    send(line) {
      const request = JSON.parse(line);
      sent.push(request);
      if (request.method === "initialize")
        receive(
          JSON.stringify({
            jsonrpc: "2.0",
            id: request.id,
            result: {
              protocolVersion: 1,
              serverInfo: { name: "fixture", version: "test" },
              capabilities: { logs: true },
            },
          }),
        );
    },
    onMessage(handler) {
      receive = handler;
    },
    onClose(handler) {
      closed = handler;
    },
    close() {
      closed();
    },
  };
  return { kit: await createHarnessKit({ transport }), transport, sent };
}

test(
  "aborted log setup rejects promptly and bounds remote cleanup",
  { timeout: 3000 },
  async () => {
    const { kit, transport, sent } = await unresponsiveLogsPeer();
    try {
      const abort = new AbortController();
      const reason = new Error("stop setup");
      const pending = kit.logs.subscribe(() => {}, { signal: abort.signal });
      abort.abort(reason);
      await Promise.race([
        assert.rejects(pending, (error) => error === reason),
        new Promise((_, reject) =>
          setTimeout(() => reject(new Error("setup waited for cleanup")), 100),
        ),
      ]);
      const cleanup = sent.find((request) => request.method === "logs.unsubscribe");
      assert.ok(cleanup?.id);
      // A silent peer must not leave the cleanup RPC pending indefinitely.
      await new Promise((resolve) => setTimeout(resolve, 1100));
      assert.ok(
        sent.some(
          (request) => request.method === "$/cancelRequest" && request.params?.id === cleanup.id,
        ),
      );
    } finally {
      await transport.close();
    }
  },
);

test("already-aborted log setup sends no subscribe or cleanup request", async () => {
  const { kit, transport, sent } = await unresponsiveLogsPeer();
  try {
    const abort = new AbortController();
    const reason = new Error("already stopped");
    abort.abort(reason);
    await assert.rejects(
      kit.logs.subscribe(() => {}, { signal: abort.signal }),
      (error) => error === reason,
    );
    assert.deepEqual(
      sent.map((request) => request.method),
      ["initialize"],
    );
  } finally {
    await transport.close();
  }
});
