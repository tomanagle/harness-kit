import { test } from "node:test";
import assert from "node:assert/strict";
import { serveExecution } from "../src/execution.ts";
import type { RemoteProtocol, Command } from "../src/remote.gen.ts";
const command: Command = {
  id: "c",
  sessionId: "s",
  epoch: 1,
  kind: "prompt",
  payload: { text: "hello" },
  status: "claimed",
  result: {},
};
test("lost command completion retries only the acknowledgement and stop drains execution before release", async () => {
  const order: string[] = [];
  let claimed = false,
    completed = 0;
  let admit!: () => void;
  const admitted = new Promise<void>((r) => {
    admit = r;
  });
  const protocol = {
    async acquireLease() {
      return {
        sessionId: "s",
        deviceId: "d",
        epoch: 1,
        expiresAt: new Date(Date.now() + 30000).toISOString(),
      };
    },
    async claimCommand() {
      if (claimed) return { commands: [] };
      claimed = true;
      return { commands: [command] };
    },
    async completeCommand() {
      completed++;
      if (completed === 1) throw new TypeError("lost ACK");
      admit();
      return command;
    },
    async releaseLease() {
      order.push("release");
      return {};
    },
  } as unknown as RemoteProtocol;
  const owner = await serveExecution(protocol, "s", 0, {
    async acquired() {
      order.push("persist epoch");
    },
    async handle() {
      order.push("admit");
      return {};
    },
    async lost() {
      await Promise.resolve();
      order.push("stop local");
    },
  });
  await admitted;
  await owner.stop();
  assert.equal(completed, 2);
  assert.deepEqual(order, ["persist epoch", "admit", "stop local", "release"]);
});
test("an expired lease cannot admit a command or release uncertain ownership", async () => {
  let stopped = false;
  const protocol = {
    async acquireLease() {
      return { sessionId: "s", deviceId: "d", epoch: 1, expiresAt: new Date(0).toISOString() };
    },
    async claimCommand() {
      throw Error("must not claim");
    },
    async releaseLease() {
      assert.fail("must not release");
    },
  } as unknown as RemoteProtocol;
  const owner = await serveExecution(protocol, "s", 0, {
    async handle() {
      assert.fail("must not execute");
    },
    lost() {
      stopped = true;
    },
  });
  await owner.stop();
  assert.equal(stopped, true);
});
