// Everything Harness Kit knows about, from Node.js: runtimes, your session
// history, host tools and MCP servers. The same calls the dashboards make.
//
// From core/:
//   go build -o bin/harness-kit ./cmd/harness-kit
//   node examples/node/overview.ts            # readable
//   node examples/node/overview.ts --json     # one JSON document (pipe to jq)

import { createHarnessKit } from "../../../packages/client-ts/src/index.ts";
import { stdio } from "../../../packages/client-ts/src/node.ts";
// @ts-ignore: plain JavaScript shared with the Electron and Wails examples
import { demoTools } from "../ui/demo-tools.js";

const asJSON = process.argv.includes("--json");
const bin = process.env.HARNESS_KIT_BIN ?? new URL("../../bin/harness-kit", import.meta.url).pathname;
const kit = await createHarnessKit({
  // --store default: read the same session history as the harness-kit CLI.
  transport: stdio({ command: bin, args: ["serve", "--fake-runtime", "--kit-mcp", "--store", "default", "--log-level", "warn"] }),
  clientInfo: { name: "overview-example" },
});

try {
  await kit.tools.register(demoTools); // so the tools section has something to show
  const [runtimes, sessions, tools, mcp] = await Promise.all([
    kit.runtimes.discover(),
    kit.sessions.list(),
    kit.tools.list(),
    kit.mcp.list({ cwd: process.cwd(), runtimes: ["codex", "claude"] }),
  ]);
  sessions.sort((a, b) => b.updatedAt.localeCompare(a.updatedAt));

  if (asJSON) {
    console.log(JSON.stringify({ runtimes, sessions, tools, mcp }, null, 2));
  } else {
    const ago = (iso: string) => {
      const s = (Date.now() - Date.parse(iso)) / 1000;
      return s < 60 ? "just now" : s < 3600 ? `${Math.floor(s / 60)}m ago` : s < 86400 ? `${Math.floor(s / 3600)}h ago` : `${Math.floor(s / 86400)}d ago`;
    };
    const pad = (s: string, n: number) => (s.length > n ? s.slice(0, n - 1) + "…" : s.padEnd(n));

    console.log("\nRuntimes");
    for (const d of runtimes) {
      const auth = d.auth.method ? `${d.auth.state} (${d.auth.method}${d.auth.plan ? ", " + d.auth.plan : ""})` : d.auth.state;
      console.log(`  ${d.ready ? "✓" : "✗"} ${pad(d.runtime, 8)} ${pad(d.version ?? "-", 9)} ${auth}${d.problems?.length ? "  - " + d.problems[0] : ""}`);
    }

    console.log(`\nSessions (${sessions.length}, newest first)`);
    if (!sessions.length) console.log('  none yet: try  ./hk prompt -runtime fake "hello"');
    for (const s of sessions.slice(0, 15)) {
      console.log(`  ${s.sessionId}  ${pad(s.runtime, 7)} ${pad(`${s.turns} turn${s.turns === 1 ? "" : "s"}`, 9)} ${pad(ago(s.updatedAt), 9)} ${s.title ?? "(no prompt yet)"}`);
    }
    if (sessions.length > 15) console.log(`  … and ${sessions.length - 15} more (./hk sessions --limit 0)`);

    console.log("\nHost tools (registered by this example)");
    for (const t of tools) {
      const flags = [t.approval === "ask" ? "asks" : "", t.readOnly ? "read-only" : ""].filter(Boolean).join(", ");
      console.log(`  ${pad(t.name, 14)} ${t.description ?? ""}${flags ? `  [${flags}]` : ""}`);
    }

    console.log("\nMCP servers");
    if (!mcp.enabled) console.log("  kit config is off (start the sidecar with --kit-mcp)");
    else if (!mcp.servers.length) console.log("  no kit servers: add one with  ./hk mcp add NAME -- COMMAND");
    for (const e of mcp.servers) {
      const target = e.server.url ?? [e.server.command, ...(e.server.args ?? [])].join(" ");
      console.log(`  ${pad(e.name, 14)} ${pad(e.source, 8)} ${pad(target, 50)} ${e.loaded ? "" : "(untrusted, not loaded)"}`);
    }
    for (const n of mcp.native) {
      if (n.supported) console.log(`  ${n.runtime}'s own: ${n.servers.join(", ") || "none"}${n.error ? "  (" + n.error + ")" : ""}`);
      else console.log(`  ${n.runtime}'s own: not listable (run \`${n.runtime} mcp list\`)`);
    }
    console.log(mcp.inheritNative ? "  runtimes also load their own servers (--no-native-mcp to stop)" : "  runtimes' own servers are off");
    console.log();
  }
} finally {
  await kit.close();
}
