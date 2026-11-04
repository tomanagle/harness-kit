# Harness Kit

Build your own AI agent harness applications with a standalone SDK, local runtime
adapters, session persistence, approvals and configurable settings.

This repository has two product components:

| Component | Source directories | Status |
| --- | --- | --- |
| SDK for building harness applications | [`core/`](core/README.md) contains the Go packages, local runtimes, CLI/sidecar and examples; [`packages/`](packages) contains clients, including the TypeScript package | Usable independently today |
| Server for coordinating sessions across devices | [`server/`](server/README.md) | Planned placeholder; no implementation yet |

The kit can power applications you distribute to your own users without a Harness
Kit server. Its existing `harness-kit serve` command is a local stdio sidecar for
an application; cross-device coordination belongs to the planned server product.

Build the current source checkout:

```sh
git clone https://github.com/tomanagle/harness-kit.git
cd harness-kit
make build                         # writes core/bin/harness-kit
./hk prompt --runtime fake "Hello" # offline example
```

The root `Makefile` and `hk` forward to `core/`. Go commands run inside that
directory, for example `cd core && go test ./...`. See the
[SDK guide](core/README.md) for Go, TypeScript, Electron and Wails examples, and
[configuration](core/docs/configuration.md) for startup and runtime settings.

Current Go source uses `github.com/tomanagle/harness-kit/core`; its new module paths
have not been tagged yet. Build from a checkout until a `core/v…` release exists.
Existing root-module `v0.2.0` tags and the published npm package
`@tomanagle/harness-kit@0.2.0` remain available. The npm package name is unchanged.
See [release and migration details](core/docs/releases.md).

[MIT license](LICENSE).
