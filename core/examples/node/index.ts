// Drives the harness-kit sidecar from Node.js with the TypeScript client:
// the same code an Electron main process runs.
//
// From core/:
//   go build -o bin/harness-kit ./cmd/harness-kit
//   node examples/node/index.ts fake "[approve] hello"
//   node examples/node/index.ts codex "Explain this repository in one paragraph"
//
// (Node >= 22.18 runs TypeScript directly. Published apps import
// "@tomanagle/harness-kit" and "@tomanagle/harness-kit/node" instead.)

import { createInterface } from "node:readline/promises";
import { createHarnessKit } from "../../../packages/client-ts/src/index.ts";
import { stdio } from "../../../packages/client-ts/src/node.ts";

const [runtime = "fake", ...words] = process.argv.slice(2);
const prompt = words.join(" ") || "Hello!";
const bin = process.env.HARNESS_KIT_BIN ?? new URL("../../bin/harness-kit", import.meta.url).pathname;

const kit = await createHarnessKit({
  transport: stdio({ command: bin, args: ["serve", "--fake-runtime", "--log-level", "warn"] }),
  clientInfo: { name: "node-example", version: "0.1.0" },
});
const rl = createInterface({ input: process.stdin, output: process.stderr });

try {
  for (const d of await kit.runtimes.discover()) {
    console.error(`${d.ready ? "✓" : "✗"} ${d.runtime} ${d.version ?? ""} ${d.auth.state}${d.problems?.length ? " - " + d.problems.join("; ") : ""}`);
  }

  const session = await kit.sessions.create({ runtime, cwd: process.cwd() });
  const ac = new AbortController();
  process.once("SIGINT", () => ac.abort()); // Ctrl-C cancels the turn

  for await (const event of session.prompt(prompt, { signal: ac.signal })) {
    switch (event.type) {
      case "message.delta":
        process.stdout.write(event.data.text);
        break;
      case "tool.started":
        console.error(`\n· ${event.data.title ?? event.data.name}`);
        break;
      case "approval.requested": {
        const answer = await rl.question(`\n⚠ ${event.data.title} - allow? [y/N] `);
        await kit.approvals.respond(event.data.approvalId, answer.trim().toLowerCase() === "y" ? "allow" : "deny");
        break;
      }
      case "turn.completed":
        console.error(`\n— ${event.data.status}${event.data.error ? ": " + event.data.error.message : ""}`);
        break;
    }
  }
  await session.close();
} finally {
  rl.close();
  await kit.close();
}
