# Local validation record

Validated on 2026-11-07 against disposable Postgres 16 instances and local Docker.
This records the current development checkout; it is not a production capacity
claim or an independent security audit.

- Core: `go -C core test -race ./...` passed.
- Server: all race-enabled tests passed with `HK_TEST_DATABASE_URL`, including
  separate replica/pool failover, transaction rollback, tenant/application/private
  isolation, metadata CAS and tombstones, execution fencing, uncertain commands,
  role changes, device inventory, verification/reset/revocation, shared rate limits,
  SMTP delivery, concurrent mail workers, and cleanup.
- Go client: race-enabled tests passed, including immutable destinations, bounded
  disk replay, restart after lost ACK, lease recovery and concurrent token rotation.
- TypeScript: type checking and 75 tests passed. Tests include encrypted credential
  persistence, durable commands/metadata, rejected uploads, lease expiry, lost
  completion ACKs, and disk-write failures.
- Sprint: lint and 22 unit/service tests passed. `npm run smoke:sync` passed with
  205 persisted events from the real Electron flow, two independent servers,
  encrypted credentials, revisioned rename, and a prompt queued through the UI.
  See [the detailed scope](sprint-validation.md).
- Formatting, lint, Go vet, architecture rules, and generated-contract checks passed.
- Govulncheck found no reachable vulnerable symbols or imported vulnerable packages.
  It reports the unimported OpenPGP package advisory GO-2026-5932 in the x/crypto
  module; this server uses Argon2, not OpenPGP.

The three-replica nginx fixture passed all k6 thresholds for sync, replay, password
authentication and long polling. The long-poll fixture now preallocates enough
virtual users for its intentional wait duration; the original allocation caused a
load-generator dropped iteration, which correctly failed the test. The rerun
passed. A separate sync run passed while one application replica was stopped.
The Go failover test also checks duplicate retries after losing the accepting
replica following a committed upload.

`make server-restore-test` restored the fixture into a separate temporary database
and matched row counts and content fingerprints for all 19 public tables. The
source database was untouched; the temporary restore database was removed.
This tests logical backup completeness, not a cloud provider's WAL/PITR failover.

Reproduce load and restore checks using [the k6 guide](../load/README.md).
CI runs the Go/Postgres integration suite, generated-contract checks, all four
k6 workloads, and the isolated restore check. CI results are only established
when the workflow runs on the pushed revision; these local results do not imply
that a commit was pushed or a package published.
