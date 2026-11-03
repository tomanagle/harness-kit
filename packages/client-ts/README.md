# @harness-kit/kit

The TypeScript client for [Harness Kit](https://github.com/tomanagle/harness-kit):
a Go library and sidecar that gives applications one API for coding-agent
sessions, streamed events, approvals, tools, and settings.

```sh
npm install @harness-kit/kit
go install github.com/tomanagle/harness-kit/cmd/harness-kit@latest
```

The npm package contains the client. The `harness-kit` sidecar and the agent CLI
you choose must be installed separately. Node.js 22.18 or newer is required for
the Node transport.

```ts
import { createHarnessKit } from "@harness-kit/kit";
import { stdio } from "@harness-kit/kit/node";

const kit = await createHarnessKit({
  transport: stdio({
    command: "harness-kit",
    settings: {
      process: { store: "./sessions", runtimes: ["codex"] },
      memory: { evictAfterIdleMs: 600_000 },
    },
  }),
});

try {
  const session = await kit.sessions.create({ runtime: "codex" });
  for await (const event of session.prompt("Explain this project")) {
    if (event.type === "message.delta") process.stdout.write(event.data.text);
  }

  // Apply live settings through the same settings object used at startup.
  await kit.settings.configure({
    patch: { memory: { evictAfterIdleMs: 300_000 } },
  });
} finally {
  await kit.close();
}
```

`@harness-kit/kit` has no Node.js imports or runtime dependencies. Bring your own
`Transport` for browsers and other hosts; `@harness-kit/kit/node` supplies a stdio
transport for Node and Electron's main process.

Settings describe their application timing, including live changes, next-turn
changes, and process wiring that requires a restart. See the
[configuration guide](https://github.com/tomanagle/harness-kit/blob/main/docs/configuration.md),
[protocol](https://github.com/tomanagle/harness-kit/blob/main/docs/protocol.md), and
[examples](https://github.com/tomanagle/harness-kit/tree/main/examples).
