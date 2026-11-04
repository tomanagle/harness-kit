// Host tools: register functions from Node and let an agent call them.
//
// From the repository root:
//   go -C core build -o bin/harness-kit ./cmd/harness-kit
//   node examples/node/tools.ts fake      # offline, no account needed
//   node examples/node/tools.ts codex     # real Codex (uses a little quota)
//   node examples/node/tools.ts claude    # real Claude Code
//
// The handlers below run in this Node process. The runtime reaches them
// through Harness Kit's MCP bridge; "refund" asks for approval first.

import { createInterface } from "node:readline/promises";
import { createHarnessKit } from "../../packages/client-ts/src/index.ts";
import { stdio } from "../../packages/client-ts/src/node.ts";

const runtime = process.argv[2] ?? "fake";
const bin = process.env.HARNESS_KIT_BIN ?? new URL("../../core/bin/harness-kit", import.meta.url).pathname;
const kit = await createHarnessKit({
  transport: stdio({ command: bin, args: ["serve", "--fake-runtime", "--store", "memory", "--log-level", "warn"] }),
});
const rl = createInterface({ input: process.stdin, output: process.stderr });

const orders: Record<string, string> = { A7: "shipped", B2: "processing" };
await kit.tools.register([
  {
    name: "order_status",
    description: "Look up the status of an order by ID",
    inputSchema: { type: "object", properties: { id: { type: "string", description: "Order ID, e.g. A7" } }, required: ["id"] },
    readOnly: true,
    handler: ({ id }: { id: string }) => {
      console.error(`  [handler] order_status(${id})`);
      return { id, status: orders[id] ?? "unknown" };
    },
  },
  {
    name: "refund",
    description: "Refund an order",
    inputSchema: { type: "object", properties: { id: { type: "string" } }, required: ["id"] },
    approval: "ask",
    handler: ({ id }: { id: string }) => {
      console.error(`  [handler] refund(${id})`);
      return `Order ${id} refunded`;
    },
  },
]);

// The fake runtime calls tools through a [call NAME {args}] directive;
// real runtimes decide for themselves.
const prompt =
  runtime === "fake"
    ? '[call order_status {"id":"A7"}] [call refund {"id":"A7"}]'
    : "What is the status of order A7? If it has shipped, refund it. Use the order_status and refund tools.";

const session = await kit.sessions.create({
  runtime,
  cwd: process.cwd(),
  mcp: { inheritNative: false }, // only our tools, not your native MCP servers
  ...(runtime === "claude" ? { model: "haiku", options: { tools: [] } } : {}),
  ...(runtime === "codex" ? { options: { sandbox: "read-only", effort: "low" } } : {}),
});

try {
  for await (const ev of session.prompt(prompt)) {
    switch (ev.type) {
      case "tool.started":
        console.error(`· ${ev.data.title ?? ev.data.name}`);
        break;
      case "tool.completed":
        console.error(`  → ${ev.data.status}: ${ev.data.output ?? ev.data.error ?? ""}`);
        break;
      case "approval.requested": {
        const a = await rl.question(`⚠ ${ev.data.title} - allow? [y/N] `);
        await kit.approvals.respond(ev.data.approvalId, a.trim().toLowerCase() === "y" ? "allow" : "deny");
        break;
      }
      case "message.delta":
        process.stdout.write(ev.data.text);
        break;
      case "turn.completed":
        console.error(`\n— ${ev.data.status}`);
        break;
    }
  }
} finally {
  rl.close();
  await kit.close();
}
