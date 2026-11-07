import http from "k6/http";
import { check, fail } from "k6";
import exec from "k6/execution";
import { Rate } from "k6/metrics";

const base = (__ENV.BASE_URL || "http://localhost:8090").replace(/\/$/, "");
const rate = Number(__ENV.RATE || 10);
const maxVUs = Number(__ENV.MAX_VUS || 50);
const tenants = Number(__ENV.TENANTS || 4);
const workload = __ENV.WORKLOAD || "sync";
if (!["sync", "replay", "auth", "poll"].includes(workload)) throw Error("Invalid WORKLOAD");
for (const n of [rate, maxVUs, tenants])
  if (!Number.isSafeInteger(n) || n < 1) throw Error("Limits must be positive integers");
const integrity = new Rate("sync_integrity");
export const options = {
  setupTimeout: "120s",
  scenarios: {
    load: {
      executor: "constant-arrival-rate",
      rate,
      timeUnit: "1s",
      duration: __ENV.DURATION || "30s",
      preAllocatedVUs: Math.min(workload === "poll" ? Math.max(10, rate * 2) : 10, maxVUs),
      maxVUs,
      exec: "run",
    },
  },
  thresholds: {
    http_req_failed: ["rate<0.01"],
    "http_req_duration{phase:load}": [
      `p(95)<${Number(__ENV.P95_MS || (workload === "poll" ? 2000 : 750))}`,
    ],
    checks: ["rate==1"],
    sync_integrity: ["rate==1"],
    dropped_iterations: ["count==0"],
  },
};
function request(path, body, token, phase = "setup") {
  const response = http.post(`${base}/v1/${path}`, JSON.stringify(body), {
    headers: {
      "Content-Type": "application/json",
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    redirects: 0,
    timeout: "35s",
    tags: { name: path, phase },
  });
  if (!check(response, { "request succeeded": (r) => r.status === 200 }))
    fail(`HTTP ${response.status} on ${path}`);
  return response.json();
}
export function setup() {
  const runId = `${Date.now()}-${Math.floor(Math.random() * 1e9)}`;
  const fixtures = [];
  for (let i = 0; i < tenants; i++) {
    const credentials = {
      email: `k6-${runId}-${i}@example.invalid`,
      password: `load-test-only-${runId}-${i}`,
    };
    const account = request("auth/register", credentials);
    const tenant = request("tenants/list", {}, account.accessToken).tenants[0];
    const device = request("devices/begin", { applicationId: "k6", name: "load fixture" });
    request(
      "devices/approve",
      { userCode: device.userCode, tenantId: tenant.id },
      account.accessToken,
    );
    const grant = request("devices/poll", { deviceCode: device.deviceCode });
    const seed = `seed-${runId}-${i}`;
    request(
      "sync/push",
      {
        operationId: seed,
        sessionId: seed,
        expectedSeq: 0,
        title: "Replay fixture",
        visibility: "private",
        events: Array.from({ length: 100 }, (_, seq) => ({ seq: seq + 1, text: "x".repeat(512) })),
      },
      grant.accessToken,
    );
    fixtures.push({ credentials, grant, seed });
  }
  return { runId, fixtures };
}
export function run(data) {
  const fixture = data.fixtures[exec.vu.idInTest % data.fixtures.length];
  const token = fixture.grant.accessToken;
  let valid;
  if (workload === "auth") {
    valid = !!request("auth/login", fixture.credentials, "", "load").accessToken;
  } else if (workload === "replay") {
    const page = request(
      "sync/events",
      { sessionId: fixture.seed, after: 0, limit: 100 },
      token,
      "load",
    );
    valid = page.events.length === 100 && page.nextAfter === 100 && page.events[99].seq === 100;
  } else if (workload === "poll") {
    const head = request("sync/changes", { after: 0, limit: 100, waitMs: 0 }, token, "load");
    const next = request(
      "sync/changes",
      { after: head.cursor, limit: 100, waitMs: 1000 },
      token,
      "load",
    );
    valid = next.cursor >= head.cursor;
  } else {
    const id = `s-${data.runId}-${exec.scenario.iterationInTest}`;
    const batch = {
      operationId: id,
      sessionId: id,
      expectedSeq: 0,
      title: "Load test",
      visibility: "private",
      events: [{ type: "message", text: "x".repeat(1024) }],
    };
    const ack = request("sync/push", batch, token, "load");
    const retry = request("sync/push", batch, token, "load");
    const page = request("sync/events", { sessionId: id, after: 0, limit: 100 }, token, "load");
    const feed = request(
      "sync/changes",
      { after: ack.cursor - 1, limit: 1, waitMs: 0 },
      token,
      "load",
    );
    valid =
      ack.lastSeq === 1 &&
      retry.cursor === ack.cursor &&
      page.events.length === 1 &&
      page.events[0].data.text === batch.events[0].text &&
      feed.changes[0]?.sessionId === id;
  }
  integrity.add(valid);
  check(valid, { "data and retry invariants hold": (value) => value });
}
