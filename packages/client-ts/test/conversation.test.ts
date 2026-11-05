import test from "node:test";
import assert from "node:assert/strict";
import { Conversation, ConversationState, type HarnessEvent } from "../src/index.ts";
function event(seq: number, type: string, data: unknown, turnId = "turn"): HarnessEvent {
  return {
    sessionId: "s",
    turnId,
    seq,
    type,
    data,
    time: new Date().toISOString(),
  } as HarnessEvent;
}
test("conversation projection deduplicates, completes streamed messages and separates children", () => {
  const state = new ConversationState("s");
  const events = [
    event(1, "turn.started", { text: "Prompt", input: [{ type: "text", text: "Prompt" }] }),
    event(2, "message.delta", { itemId: "m", text: "Hel" }),
    event(3, "message.completed", { itemId: "m", text: "Hello" }),
    event(4, "message.completed", { itemId: "m", parentCallId: "child", text: "Child" }),
    event(5, "subagent.updated", {
      id: "pending",
      callId: "call",
      status: "running",
      title: "Task",
    }),
    event(6, "subagent.updated", {
      id: "real",
      callId: "call",
      status: "completed",
      output: "Done",
    }),
    event(7, "approval.requested", {
      approvalId: "a",
      turnId: "turn",
      title: "Proceed?",
      kind: "command",
    }),
    event(8, "turn.completed", { status: "completed" }),
  ];
  for (const e of events) state.apply(e);
  for (const e of events) state.apply(e);
  const snapshot = state.getSnapshot();
  assert.equal(snapshot.messages.length, 3);
  assert.equal(snapshot.messages[1]?.text, "Hello");
  assert.equal(snapshot.messages[2]?.parentCallId, "child");
  assert.equal(snapshot.subtasks.length, 1);
  assert.equal(snapshot.subtasks[0]?.title, "Task");
  assert.equal(snapshot.approvals.length, 0);
  snapshot.messages[0]!.text = "changed";
  assert.equal(state.getSnapshot().messages[0]?.text, "Prompt");
});
test("conversation window is bounded and reports truncation", () => {
  const state = new ConversationState("s", { maxEvents: 2, maxBytes: 1000 });
  for (let i = 1; i <= 100; i++)
    state.apply(event(i, "message.completed", { itemId: String(i), text: "message" }));
  assert.equal(state.getSnapshot().messages.length, 2);
  assert.equal(state.getSnapshot().truncated, true);
});
test("observer merges overlapping live updates with replay and cleans up", async () => {
  let listener: (e: HarnessEvent) => void = () => {};
  let removed = false;
  let first = true;
  const records = [event(1, "message.delta", { itemId: "m", text: "Hel" })];
  const source = {
    onEvent: (fn: typeof listener) => {
      listener = fn;
      return () => {
        removed = true;
      };
    },
    sessions: {
      eventsPage: async (_id: string, opts: { afterSeq: number }) => {
        const page = records.filter((e) => e.seq > opts.afterSeq);
        if (first) {
          first = false;
          const next = event(2, "message.completed", { itemId: "m", text: "Hello" });
          records.push(next);
          listener(next);
        }
        return {
          events: page,
          hasMore: false,
          nextSeq: page.at(-1)?.seq ?? opts.afterSeq,
          truncated: false,
        };
      },
    },
  };
  const observer = new Conversation(source, "s");
  await observer.ready;
  assert.equal(observer.getSnapshot().messages[0]?.text, "Hello");
  assert.equal(observer.getSnapshot().seq, 2);
  const final = event(3, "turn.completed", { status: "completed" });
  records.push(final);
  listener(event(0, "stream.gap", { fromSeq: 3, toSeq: 3 }));
  await observer.refresh();
  assert.equal(observer.getSnapshot().turns[0]?.status, "completed");
  observer.close();
  observer.close();
  assert.equal(removed, true);
});
