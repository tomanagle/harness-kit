# Protocol schema and generated clients

The TypeScript and Go clients share one generated contract:

```text
core/rpc/protocol + referenced Go domain types + protocolgen method registry
                               ↓
                     protocol/schema.json
                               ↓
           TypeScript wire types/methods + Go wire types/methods
```

[`schema.json`](schema.json) uses JSON Schema draft 2020-12 and carries the
method, event and error metadata needed by the generators. The source of truth
is the server's Go definitions and the registry in
[`core/cmd/protocolgen`](../core/cmd/protocolgen). The schema is a checked-in,
language-neutral artifact; do not edit it or generated client files by hand.

From the repository root:

```sh
make generate         # regenerate the schema and both clients
make check-generated  # fail if committed output differs from generation
make test-clients     # test the TypeScript and standalone Go clients
```

When adding or changing an RPC, update the Go definitions and method registry,
regenerate, and commit the changed contract and generated clients together.
CI runs the generation check and client tests. The generator uses only the Go
standard library and is a development tool; consumers install the generated
code without needing it.

Generated files:

- [`packages/client-ts/src/protocol.ts`](../packages/client-ts/src/protocol.ts): wire types, event payloads and constants.
- [`packages/client-ts/src/methods.gen.ts`](../packages/client-ts/src/methods.gen.ts): typed RPC methods.
- [`packages/client-go/protocol.gen.go`](../packages/client-go/protocol.gen.go): standalone Go wire types and constants.
- [`packages/client-go/methods.gen.go`](../packages/client-go/methods.gen.go): typed RPC methods.

Connection lifecycle, transport adapters, streaming conveniences and tool
callbacks remain handwritten. The [protocol behavior guide](../core/docs/protocol.md)
defines ordering, cancellation and replay in addition to the schema's data shapes.
The existing `core/rpc/client` remains available for compatibility; new Go
applications can use the independent [Go client module](../packages/client-go/README.md).
