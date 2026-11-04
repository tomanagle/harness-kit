# Harness Kit

Build your own AI agent harness applications with a standalone SDK, local runtime
adapters, session persistence, approvals and configurable settings.

This repository has two product components:

| Component | Source directories | Status |
| --- | --- | --- |
| SDK for building harness applications | [`core/`](core/README.md) contains the Go packages, local runtimes and CLI/sidecar; [`packages/`](packages) contains TypeScript and Go clients | Usable independently today |
| Server for coordinating sessions across devices | [`server/`](server/README.md) | Planned placeholder; no implementation yet |

Runnable applications live in root [`examples/`](examples), alongside `core/` and
`packages/`. They demonstrate the SDK without adding another product component.

The kit can power applications you distribute to your own users without a Harness
Kit server. Its existing `harness-kit serve` command is a local stdio sidecar for
an application; cross-device coordination belongs to the planned server product.

Build the current source checkout:

```sh
git clone https://github.com/tomanagle/harness-kit.git
cd harness-kit
make build                         # writes core/bin/harness-kit
./hk prompt --runtime fake "Hello" # offline local runtime
go -C examples/cli run . -runtime fake "Hello" # standalone Go example
```

The root `Makefile` and `hk` forward to `core/`. Core Go commands run inside that
directory, for example `cd core && go test ./...`. See the
[SDK guide](core/README.md) for Go, TypeScript, Electron and Wails examples, and
[configuration](core/docs/configuration.md) for startup and runtime settings.
`make example <name>` runs the example applications; `make test-examples` tests
their separate Go modules.

The [TypeScript client](packages/client-ts/README.md) and standalone
[Go client](packages/client-go/README.md) have generated wire types and methods
from one [protocol schema](protocol/README.md). Run `make generate` after a
protocol change; `make check-generated` checks that committed output is current.
Their transports and application-level helpers remain handwritten.

Current Go source uses `github.com/tomanagle/harness-kit/core`; its new module paths
have not been tagged yet. Build from a checkout until a `core/v…` release exists.
Existing root-module `v0.2.0` tags and the published npm package
`@tomanagle/harness-kit@0.2.0` remain available. The npm package name is unchanged.
See [release and migration details](core/docs/releases.md).

[MIT license](LICENSE).
