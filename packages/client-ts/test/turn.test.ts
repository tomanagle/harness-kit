import test from "node:test";
import assert from "node:assert/strict";
import {
  Turn,
  EventBufferOverflowError,
  type HarnessKit,
  type HarnessEvent,
} from "../src/index.ts";
function fixture(options = {}) {
  let listener: (e: HarnessEvent) => void = () => {};
  let sent = 0;
  const kit = {
    onEvent: (fn: typeof listener) => {
      listener = fn;
      return () => {
        listener = () => {};
      };
    },
    onClose: () => () => {},
    protocol: {
      sessionPrompt: async () => {
        sent++;
      },
      sessionCancel: async () => {},
    },
  } as unknown as HarnessKit;
  const turn = new Turn(
    kit,
    "s",
    "t",
    [{ type: "text", text: "hello" }],
    undefined,
    undefined,
    undefined,
    options,
  );
  let seq = 0;
  return {
    turn,
    kit,
    sent: () => sent,
    emit(type: string, data = {}) {
      listener({
        sessionId: "s",
        turnId: "t",
        seq: ++seq,
        time: new Date().toISOString(),
        type,
        data,
      } as HarnessEvent);
    },
  };
}
test("result-only turns retain no event payloads", async () => {
  const f = fixture({ mode: "none" });
  for (let i = 0; i < 10000; i++) f.emit("message.delta", { itemId: "m", text: "delta" });
  assert.equal(f.turn.bufferedEvents, 0);
  assert.equal(f.turn.bufferedBytes, 0);
  f.emit("turn.completed", { status: "completed" });
  assert.equal((await f.turn.done).status, "completed");
});
test("iterator overflow is explicit while the terminal result remains available", async () => {
  const f = fixture({ maxEvents: 2, maxBytes: 10000 });
  for (let i = 0; i < 3; i++) f.emit("message.delta", { itemId: "m", text: "delta" });
  assert.equal(f.turn.bufferedEvents, 0);
  await assert.rejects(async () => {
    for await (const _event of f.turn) {
    }
  }, EventBufferOverflowError);
  f.emit("turn.completed", { status: "completed" });
  assert.equal((await f.turn.done).status, "completed");
});
test("breaking iteration discards subsequent events without cancelling the turn", async () => {
  const f = fixture();
  f.emit("message.delta", { itemId: "m", text: "delta" });
  for await (const _event of f.turn) break;
  for (let i = 0; i < 1000; i++) f.emit("message.delta", { itemId: "m", text: "delta" });
  assert.equal(f.turn.bufferedEvents, 0);
  f.emit("turn.completed", { status: "completed" });
  await f.turn.done;
  await assert.rejects(async () => {
    for await (const _event of f.turn) {
    }
  }, /once/);
});
test("byte limits and pre-aborted prompts are enforced before sending work", async () => {
  const f = fixture({ maxBytes: 20 });
  f.emit("message.delta", { itemId: "m", text: "large" });
  await assert.rejects(async () => {
    for await (const _event of f.turn) {
    }
  }, EventBufferOverflowError);
  f.emit("turn.completed", { status: "completed" });
  await f.turn.done;
  const controller = new AbortController();
  controller.abort(new Error("not started"));
  const before = f.sent();
  const turn = new Turn(f.kit, "s", "new", [], controller.signal);
  await assert.rejects(turn.done, /not started/);
  assert.equal(f.sent(), before);
  assert.throws(() => fixture({ maxEvents: -1 }), RangeError);
});

test("recovery advances over unrelated history and fails explicitly on an unrecoverable live event", async () => {
  const f = fixture({ mode: "none" });
  const cursors: number[] = [];
  Object.assign(f.kit, {
    sessions: {
      eventsPage: async (_id: string, { afterSeq }: { afterSeq: number }) => {
        cursors.push(afterSeq);
        if (cursors.length === 1) {
          f.emit("message.delta", { itemId: "m", text: "missing from retention" });
          return { events: [], nextSeq: 10, hasMore: false, truncated: true };
        }
        return { events: [], nextSeq: afterSeq, hasMore: false, truncated: false };
      },
    },
  });
  f.emit("stream.gap");
  await assert.rejects(f.turn.done, /cannot recover/);
  assert.deepEqual(cursors, [0, 10]);
});
