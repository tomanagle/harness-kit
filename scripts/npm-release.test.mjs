import assert from "node:assert/strict";
import test from "node:test";
import { planRelease, readVersions, versionManifests } from "./npm-release.mjs";

const source = { commit: "a".repeat(40), clientTree: "b".repeat(40) };
function input(overrides = {}) {
  const pkg = { name: "@tomanagle/harness-kit", version: "0.2.0", files: ["dist", "src"] };
  const lock = {
    name: pkg.name,
    version: pkg.version,
    packages: { "": { name: pkg.name, version: pkg.version }, node_modules: { version: "keep" } },
  };
  return { pkg, lock, source, automatic: true, versions: { "0.2.0": {} }, ...overrides };
}
function setVersion(args, version) {
  args.pkg.version = args.lock.version = args.lock.packages[""].version = version;
  return args;
}

test("automatic release increments the highest stable patch, not lexicographic order or prereleases", () => {
  assert.equal(planRelease(input()).version, "0.2.1");
  assert.equal(
    planRelease(input({ versions: { "0.2.9": {}, "0.2.10": {}, "1.0.0-rc.1": {} } })).version,
    "0.2.11",
  );
});

test("a deliberate version floor wins and an unpublished package can bootstrap", () => {
  assert.equal(planRelease(setVersion(input(), "0.3.0")).version, "0.3.0");
  assert.equal(planRelease(input({ versions: {} })).version, "0.2.0");
});

test("successful retries and unrelated commits do not create duplicate client releases", () => {
  const args = input({ versions: { "0.2.1": { harnessKitRelease: source } } });
  const plan = planRelease(args);
  assert.equal(plan.publish, false);
  assert.equal(plan.version, "0.2.1");
  assert.equal(
    planRelease({ ...args, source: { ...source, commit: "c".repeat(40) } }).publish,
    false,
  );
});

test("a changed client revision advances version and retains ancestry information", () => {
  const old = { commit: "d".repeat(40), clientTree: "e".repeat(40) };
  const plan = planRelease(input({ versions: { "0.2.1": { harnessKitRelease: old } } }));
  assert.equal(plan.version, "0.2.2");
  assert.equal(plan.previousCommit, old.commit);
});

test("explicit tagged prereleases go to next and require a matching manifest", () => {
  const args = setVersion(input({ automatic: false, refTag: "npm-v0.3.0-rc.1" }), "0.3.0-rc.1");
  assert.equal(planRelease(args).tag, "next");
  assert.throws(() => planRelease({ ...args, refTag: "npm-v0.3.0" }), /tag must match/);
  assert.throws(() => planRelease({ ...args, automatic: true }), /stable version floor/);
});

test("manual retries skip the same client revision but reject version collisions", () => {
  assert.throws(() => planRelease(input({ automatic: false })), /already exists/);
  assert.equal(
    planRelease(input({ automatic: false, versions: { "0.2.0": { harnessKitRelease: source } } }))
      .publish,
    false,
  );
  const args = setVersion(input({ automatic: false, versions: { "0.3.0": {} } }), "0.2.1");
  assert.throws(() => planRelease(args), /advance latest/);
});

test("identity, lock consistency and source hashes fail closed", () => {
  const args = input();
  args.lock.packages[""].version = "wrong";
  assert.throws(() => planRelease(args));
  assert.throws(() => planRelease({ ...input(), source: { ...source, commit: "main" } }));
  assert.throws(() => planRelease(setVersion(input(), "01.2.0")));
  const other = input();
  other.pkg.name = "@other/package";
  assert.throws(() => planRelease(other));
});

test("release manifests change both root versions without changing dependencies or caller inputs", () => {
  const args = input();
  const next = versionManifests(args.pkg, args.lock, planRelease(args), source);
  assert.equal(next.pkg.version, "0.2.1");
  assert.deepEqual(next.pkg.harnessKitRelease, source);
  assert.equal(next.lock.version, "0.2.1");
  assert.equal(next.lock.packages[""].version, "0.2.1");
  assert.deepEqual(next.lock.packages.node_modules, args.lock.packages.node_modules);
  assert.equal(args.pkg.version, "0.2.0");
  assert.equal(args.lock.packages[""].version, "0.2.0");
});

test("registry lookup distinguishes missing packages from outages and malformed responses", async () => {
  assert.deepEqual(await readVersions(async () => new Response("", { status: 404 })), {});
  await assert.rejects(
    readVersions(async () => new Response("", { status: 503 })),
    /HTTP 503/,
  );
  await assert.rejects(
    readVersions(async () => {
      throw new Error("offline");
    }),
    /offline/,
  );
  await assert.rejects(
    readVersions(async () => Response.json({ name: "@tomanagle/harness-kit" })),
    /no versions/,
  );
  assert.deepEqual(
    await readVersions(async () =>
      Response.json({ name: "@tomanagle/harness-kit", versions: { "0.2.0": {} } }),
    ),
    { "0.2.0": {} },
  );
});
