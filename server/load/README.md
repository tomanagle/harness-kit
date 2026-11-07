# Load and failover tests

From the repository root, with Docker running:

```sh
make server-cluster-up
make server-load-test
WORKLOAD=replay RATE=50 DURATION=60s make server-load-test
WORKLOAD=auth RATE=3 DURATION=30s make server-load-test
WORKLOAD=poll RATE=10 DURATION=30s MAX_VUS=100 make server-load-test
make server-cluster-down
```

The fixture runs three identical read-only server containers behind nginx, with
no sticky sessions, and one disposable Postgres database. It binds only loopback
ports 8090 and 54339. Override `HARNESS_SERVER_PORT` / `HARNESS_POSTGRES_PORT` if
needed. It is a test topology, not an HA Postgres deployment. Local development
passwords must never be used for a public deployment. `cluster-down` preserves
test data; use `docker compose -f server/deploy/compose.yml down --volumes` to
remove **this fixture's** data.

Each run registers isolated accounts, approves device grants, and seeds transcripts.
`TENANTS` defaults to 4. It never needs production credentials. Public registration
must be enabled; the fixture raises the shared auth rate limit for load generation.
Do not point these workloads at a production service.

| WORKLOAD | Operations per iteration                                             |
| -------- | -------------------------------------------------------------------- |
| `sync`   | Append, retry the identical operation, replay, validate ordered feed |
| `replay` | Fetch and validate 100 seeded events                                 |
| `auth`   | Password login with Argon2id                                         |
| `poll`   | Read cursor, then long-poll for up to one second                     |

`RATE` is iterations started per second, **not requests per second**. The
[constant-arrival-rate executor](https://grafana.com/docs/k6/latest/using-k6/scenarios/executors/constant-arrival-rate/)
keeps offered work independent of response time. `MAX_VUS` bounds concurrent
virtual users. Dropped iterations fail the test rather than hiding saturation.

[Thresholds](https://grafana.com/docs/k6/latest/using-k6/thresholds/) exit nonzero on
failed checks, any data-integrity failure, dropped iterations, HTTP failure rate
of 1% or more, or load p95 above 750 ms (2 s for intentional long polls). Override
`P95_MS` when running k6 directly. `DURATION` defaults to 30s. CI runs all four modes
as correctness smoke tests and uploads their logs. Increase rate and duration on
representative hardware to establish an actual capacity envelope.

For direct k6 use:

```sh
BASE_URL=http://localhost:8090 RATE=20 DURATION=60s k6 run server/load/sync.js
```

During a run, stop one `server` replica using Docker and restart it. A severed
in-flight request may fail; k6 reports it rather than concealing availability
loss. Client workers retry immutable operations from disk. The automated Go
`TestReplicaFailover` specifically verifies a committed upload whose accepting
replica is lost, concurrent identical retries through two other replicas, exact
replay, cross-replica refresh, and revocation. Run it against disposable Postgres:

```sh
HK_TEST_DATABASE_URL=... go -C server test -race ./internal/handlers -run TestReplicaFailover
```

Inspect `docker stats` alongside k6 and Postgres connection/lock/I/O metrics.
Per-replica memory is bounded by HTTP request limits, query pages, connection pools,
and password concurrency; total RSS depends on workload and Go's collector. A
single tenant/application serializes its change cursor, so adding replicas does
not remove that database bottleneck. Load different tenant counts as well as a
single hot tenant. Test database failover separately with your chosen HA Postgres
service; this fixture cannot validate that service's durability settings.
