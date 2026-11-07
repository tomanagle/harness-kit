import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, readFile, readdir, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createCipheriv, createDecipheriv, randomBytes } from "node:crypto";
import { encryptedCredentialStore } from "../src/credentials.node.ts";
import { scopeKey } from "../src/remote.ts";
const scope = {
  serverId: "work",
  url: "https://sync.example.test",
  accountId: "a",
  tenantId: "t",
  applicationId: "sprint",
};
const grant = {
  accountId: "a",
  tenantId: "t",
  applicationId: "sprint",
  deviceId: "d",
  accessToken: "secret-access",
  refreshToken: "secret-refresh",
  expiresAt: "2030-01-01T00:00:00Z",
};
test("encrypted credentials reopen by immutable scope and fail closed without key storage", async (t) => {
  const dir = await mkdtemp(join(tmpdir(), "hk-creds-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const key = randomBytes(32);
  let available = true;
  const encryption = {
    async isAsyncEncryptionAvailable() {
      return available;
    },
    async encryptStringAsync(value: string) {
      const iv = randomBytes(12),
        cipher = createCipheriv("aes-256-gcm", key, iv);
      const ciphertext = Buffer.concat([cipher.update(value), cipher.final()]);
      return Buffer.concat([iv, cipher.getAuthTag(), ciphertext]);
    },
    async decryptStringAsync(value: Buffer) {
      const cipher = createDecipheriv("aes-256-gcm", key, value.subarray(0, 12));
      cipher.setAuthTag(value.subarray(12, 28));
      return {
        result: Buffer.concat([cipher.update(value.subarray(28)), cipher.final()]).toString(),
        shouldReEncrypt: false,
      };
    },
  };
  const store = encryptedCredentialStore(dir, encryption);
  await store.set(scopeKey(scope), grant);
  const bytes = await readFile(join(dir, (await readdir(dir))[0]!), "utf8");
  assert.ok(!bytes.includes(grant.accessToken));
  assert.ok(!bytes.includes(grant.refreshToken));
  const reopened = encryptedCredentialStore(dir, encryption);
  assert.deepEqual(await reopened.scopes(), [scope]);
  assert.deepEqual(await reopened.get(scopeKey(scope)), grant);
  assert.equal(await reopened.get(scopeKey({ ...scope, tenantId: "personal" })), undefined);
  available = false;
  await assert.rejects(reopened.get(scopeKey(scope)), /unavailable/);
  await assert.rejects(reopened.set(scopeKey(scope), grant), /unavailable/);
  available = true;
  await assert.rejects(
    encryptedCredentialStore(dir, {
      ...encryption,
      getSelectedStorageBackend: () => "basic_text",
    }).get(scopeKey(scope)),
    /unavailable/,
  );
  await reopened.delete(scopeKey(scope));
  assert.deepEqual(await reopened.scopes(), []);
});
