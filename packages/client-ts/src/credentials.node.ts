import { createHash, randomUUID } from "node:crypto";
import { mkdir, open, readFile, readdir, rename, rm } from "node:fs/promises";
import { join } from "node:path";
import { scopeKey } from "./remote.ts";
import type { CredentialStore, Grant, ServerScope } from "./remote.ts";

/** Electron safeStorage satisfies this interface. No Electron dependency enters
 * the kit. Refuse unavailable/keyless encryption rather than silently using disk plaintext. */
export interface CredentialEncryption {
  isAsyncEncryptionAvailable(): Promise<boolean>;
  encryptStringAsync(value: string): Promise<Buffer>;
  decryptStringAsync(value: Buffer): Promise<{ result: string; shouldReEncrypt: boolean }>;
  getSelectedStorageBackend?(): string;
}
export interface EncryptedCredentials extends CredentialStore {
  scopes(): Promise<ServerScope[]>;
}
export function encryptedCredentialStore(
  directory: string,
  encryption: CredentialEncryption,
): EncryptedCredentials {
  const file = (key: string) =>
    join(directory, createHash("sha256").update(key).digest("hex") + ".json");
  const ready = async () => {
    if (
      !(await encryption.isAsyncEncryptionAvailable()) ||
      encryption.getSelectedStorageBackend?.() === "basic_text"
    )
      throw new Error("Secure credential encryption is unavailable");
  };
  let tail: Promise<unknown> = Promise.resolve();
  function lock<T>(work: () => Promise<T>): Promise<T> {
    const next = tail.catch(() => {}).then(work);
    tail = next;
    return next;
  }
  function scope(key: string): ServerScope {
    const parts = JSON.parse(key);
    if (
      !Array.isArray(parts) ||
      parts.length !== 5 ||
      parts.some((part) => typeof part !== "string")
    )
      throw new Error("Invalid credential scope");
    const [serverId, url, accountId, tenantId, applicationId] = parts as string[];
    const value = {
      serverId: serverId!,
      url: url!,
      accountId: accountId!,
      tenantId: tenantId!,
      applicationId: applicationId!,
    };
    if (scopeKey(value) !== key) throw new Error("Invalid credential scope");
    return value;
  }
  async function persist(key: string, snapshot: string): Promise<void> {
    const ciphertext = (await encryption.encryptStringAsync(snapshot)).toString("base64");
    await mkdir(directory, { recursive: true, mode: 0o700 });
    const temporary = file(key) + "." + randomUUID() + ".tmp";
    try {
      const handle = await open(temporary, "wx", 0o600);
      try {
        await handle.writeFile(JSON.stringify({ version: 1, key, ciphertext }));
        await handle.sync();
      } finally {
        await handle.close();
      }
      await rename(temporary, file(key));
      if (process.platform !== "win32") {
        const dir = await open(directory, "r");
        try {
          await dir.sync();
        } finally {
          await dir.close();
        }
      }
    } finally {
      await rm(temporary, { force: true });
    }
  }
  return {
    get(key) {
      return lock(async () => {
        await ready();
        scope(key);
        try {
          const saved = JSON.parse(await readFile(file(key), "utf8"));
          if (saved.key !== key || saved.version !== 1)
            throw new Error("Invalid credential envelope");
          const decrypted = await encryption.decryptStringAsync(
            Buffer.from(saved.ciphertext, "base64"),
          );
          if (decrypted.shouldReEncrypt) await persist(key, decrypted.result);
          return JSON.parse(decrypted.result) as Grant;
        } catch (error) {
          if ((error as NodeJS.ErrnoException).code === "ENOENT") return undefined;
          throw error;
        }
      });
    },
    set(key, grant) {
      const snapshot = JSON.stringify(grant);
      return lock(async () => {
        await ready();
        scope(key);
        await persist(key, snapshot);
      });
    },
    delete(key) {
      return lock(async () => {
        scope(key);
        await rm(file(key), { force: true });
      });
    },
    scopes() {
      return lock(async () => {
        let entries: string[];
        try {
          entries = await readdir(directory);
        } catch (error) {
          if ((error as NodeJS.ErrnoException).code === "ENOENT") return [];
          throw error;
        }
        const result: ServerScope[] = [];
        for (const name of entries.filter((name) => /^[a-f0-9]{64}\.json$/.test(name))) {
          const saved = JSON.parse(await readFile(join(directory, name), "utf8"));
          const value = scope(saved.key);
          if (file(saved.key) !== join(directory, name))
            throw new Error("Invalid credential filename");
          result.push(value);
        }
        return result;
      });
    },
  };
}
