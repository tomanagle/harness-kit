# Examples

Applications built with the Go SDK in [`core/`](../core/README.md) and clients in
[`packages/`](../packages). These are separate from the future coordination server.

From the repository root:

```sh
make example                         # list available examples
make example cli RUNTIME=fake PROMPT="Hello"
make example node RUNTIME=fake PROMPT="Hello"
make test-examples                   # check the Go example modules
```

- [`cli/`](cli) links the Go SDK directly and can run interactive sessions.
- [`node/`](node) drives the local sidecar through the TypeScript client.
- [`electron/`](electron) and [`wails/`](wails) use the shared [`ui/`](ui) dashboard.
- [`model/`](model) demonstrates a caller-owned agent loop with direct model providers.

The shared desktop dashboard includes an optional source-control panel: status,
staged and working diffs, selective staging, commits, branches, history and
explicit remote operations. It uses the kit's [Git APIs](../core/docs/git.md)
in Electron, Wails sidecar mode and Wails direct mode. Hosts can replace this UI
and workflow while reusing the same primitives and settings.

Go examples have their own modules and use local replacements for the current
SDK checkout. See [the SDK guide](../core/README.md#examples) for runtime options
and [model providers](../core/docs/models.md) for the direct-model example.
