# Harness Kit Server

This directory reserves a place for a future deployable server that coordinates
Harness Kit sessions across devices. It contains no server implementation yet.
Its API, deployment model, storage and authentication are still to be designed.

The [SDK](../core/README.md), spanning `core/` and root [`packages/`](../packages),
is fully usable on its own to build and distribute harness applications. Its
existing `harness-kit serve` command is a local JSON-RPC sidecar over stdio;
it does not provide cross-device coordination.

See the [repository overview](../README.md) for the current layout.
