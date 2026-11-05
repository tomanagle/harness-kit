// Version selection is separate from publishing so it can be tested offline.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { appendFileSync, readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

const packageName = "@tomanagle/harness-kit";
const stablePattern = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/;
const versionPattern =
  /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$/;

function stableParts(version) {
  assert.match(version, stablePattern);
  const parts = version.split(".").map(Number);
  assert.ok(parts.every(Number.isSafeInteger), "version components exceed safe integer range");
  return parts;
}

function compareStable(a, b) {
  const left = stableParts(a);
  const right = stableParts(b);
  for (let i = 0; i < 3; i++) {
    if (left[i] !== right[i]) return left[i] < right[i] ? -1 : 1;
  }
  return 0;
}

export function planRelease({ pkg, lock, versions, source, automatic, refTag }) {
  assert.equal(pkg.name, packageName);
  assert.equal(pkg.private, undefined);
  assert.equal(lock.name, pkg.name);
  assert.equal(lock.version, pkg.version);
  assert.equal(lock.packages[""].name, pkg.name);
  assert.equal(lock.packages[""].version, pkg.version);
  assert.match(pkg.version, versionPattern);
  assert.match(source.commit, /^[0-9a-f]{40,64}$/);
  assert.match(source.clientTree, /^[0-9a-f]{40,64}$/);
  if (refTag) assert.equal(refTag, `npm-v${pkg.version}`, "tag must match package.json");

  const stable = Object.keys(versions)
    .filter((v) => stablePattern.test(v))
    .sort(compareStable);
  const highest = stable.at(-1);
  const previous = highest ? versions[highest] : undefined;
  let version = pkg.version;
  if (automatic) {
    assert.match(version, stablePattern, "automatic main releases require a stable version floor");
    if (previous?.harnessKitRelease?.clientTree === source.clientTree) {
      return {
        publish: false,
        version: highest,
        tag: "latest",
        reason: "Client revision already published",
      };
    }
    if (highest && compareStable(version, highest) <= 0) {
      const parts = stableParts(highest);
      parts[2]++;
      assert.ok(Number.isSafeInteger(parts[2]), "patch version overflow");
      version = parts.join(".");
    }
  }
  const tag = version.includes("-") ? "next" : "latest";
  const existing = versions[version];
  if (existing) {
    assert.equal(
      existing.harnessKitRelease?.clientTree,
      source.clientTree,
      `${version} already exists with different or unknown source; select a new version`,
    );
    return {
      publish: false,
      version,
      tag,
      reason: "Version already published from this client revision",
    };
  }
  if (tag === "latest" && highest) {
    assert.ok(compareStable(version, highest) > 0, "stable release must advance latest");
  }
  return { publish: true, version, tag, previousCommit: previous?.harnessKitRelease?.commit };
}

export async function readVersions(fetchRegistry = fetch) {
  const response = await fetchRegistry(
    `https://registry.npmjs.org/${encodeURIComponent(packageName)}`,
    {
      signal: AbortSignal.timeout(30_000),
    },
  );
  if (response.status === 404) return {};
  if (!response.ok) throw new Error(`npm registry lookup failed: HTTP ${response.status}`);
  const registry = await response.json();
  assert.equal(registry.name, packageName);
  assert.ok(
    registry.versions && typeof registry.versions === "object",
    "registry response has no versions",
  );
  return registry.versions;
}

export function versionManifests(pkg, lock, plan, source) {
  return {
    pkg: { ...pkg, version: plan.version, harnessKitRelease: source },
    lock: {
      ...lock,
      version: plan.version,
      packages: { ...lock.packages, "": { ...lock.packages[""], version: plan.version } },
    },
  };
}

async function main() {
  const git = (...args) => execFileSync("git", args, { encoding: "utf8" }).trim();
  const pkg = JSON.parse(readFileSync("package.json", "utf8"));
  const lock = JSON.parse(readFileSync("package-lock.json", "utf8"));
  const source = {
    commit: git("rev-parse", "HEAD"),
    clientTree: git("rev-parse", "HEAD:packages/client-ts"),
  };
  const automatic = process.env.AUTO_RELEASE === "true";
  const plan = planRelease({
    pkg,
    lock,
    source,
    automatic,
    versions: await readVersions(),
    refTag: process.env.GITHUB_REF_TYPE === "tag" ? process.env.GITHUB_REF_NAME : undefined,
  });
  if (automatic && plan.publish && plan.previousCommit) {
    assert.match(plan.previousCommit, /^[0-9a-f]{40,64}$/);
    // Full checkout history lets older/reordered runs fail closed rather than
    // publish older source over a newer release or a divergent release branch.
    git("merge-base", "--is-ancestor", plan.previousCommit, source.commit);
  }
  if (plan.publish) {
    const manifests = versionManifests(pkg, lock, plan, source);
    writeFileSync("package.json", JSON.stringify(manifests.pkg, null, 2) + "\n");
    writeFileSync("package-lock.json", JSON.stringify(manifests.lock, null, 2) + "\n");
  }
  const message = plan.publish
    ? `Release ${packageName}@${plan.version} to ${plan.tag}`
    : plan.reason;
  console.log(message);
  appendFileSync(
    process.env.GITHUB_OUTPUT,
    `publish=${plan.publish}\nversion=${plan.version}\ntag=${plan.tag}\n`,
  );
  appendFileSync(
    process.env.GITHUB_STEP_SUMMARY,
    `${message}. Source: \`${source.commit}\`; client tree: \`${source.clientTree}\`.\n`,
  );
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main();
}
