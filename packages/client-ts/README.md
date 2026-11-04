# @tomanagle/harness-kit

The TypeScript client for [Harness Kit](https://github.com/tomanagle/harness-kit):
a Go library and local sidecar that gives applications one API for coding-agent
sessions, streamed events, approvals, tools, and settings.

```sh
npm install @tomanagle/harness-kit
# The published v0.2.0 sidecar uses the original Go module path.
go install github.com/tomanagle/harness-kit/cmd/harness-kit@v0.2.0
```

The npm package contains the client. The `harness-kit` sidecar and the agent CLI
you choose must be installed separately. Node.js 22.18 or newer is required for
the Node transport.

The Go SDK source lives in `core/`; clients live in the root `packages/` directory.
To build the current checkout, run `make -C core build` from the repository root; the binary is
`core/bin/harness-kit`. New Go source imports use
`github.com/tomanagle/harness-kit/core/...`; see the
[module migration notes](https://github.com/tomanagle/harness-kit/blob/main/core/docs/releases.md)
before switching from the published v0.2.0 Go module. The future coordination
server is separate from this local sidecar. Runnable applications live in the
repository-root `examples/` directory; `make example node` and
`make example electron` work from the repository root.

```ts
import { createHarnessKit } from "@tomanagle/harness-kit";
import { stdio } from "@tomanagle/harness-kit/node";

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

`@tomanagle/harness-kit` has no Node.js imports or runtime dependencies. Bring your own
`Transport` for browsers and other hosts; `@tomanagle/harness-kit/node` supplies a stdio
transport for Node and Electron's main process.

Wire types, error constants and the low-level `ProtocolClient` are generated from
the shared protocol schema. The application helpers above use those generated
methods; streaming, tool handlers and transport lifetimes remain in the client
runtime. For the exact request and response shapes, use `kit.protocol`:

```ts
const { session } = await kit.protocol.sessionCreate({ runtime: "codex" });
const page = await kit.protocol.sessionEventsPage({ sessionId: session.sessionId });
```

The raw `kit.request()` method remains available for protocol extensions. In a
repository checkout, regenerate the schema and clients with `make generate`;
`make check-generated` verifies that committed output is current. The standalone
[Go client](https://github.com/tomanagle/harness-kit/tree/main/packages/client-go)
uses the same generated contract.

Settings describe their application timing, including live changes, next-turn
changes, and process wiring that requires a restart. See the
[configuration guide](https://github.com/tomanagle/harness-kit/blob/main/core/docs/configuration.md),
[protocol](https://github.com/tomanagle/harness-kit/blob/main/core/docs/protocol.md), and
[examples](https://github.com/tomanagle/harness-kit/tree/main/examples).
